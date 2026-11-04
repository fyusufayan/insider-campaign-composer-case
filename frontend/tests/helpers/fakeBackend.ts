// A small stateful fake of the CP6 HTTP API, installed as global fetch.
// Tests exercise the real client, store and components against HTTP-shaped
// responses; only the network is faked. Per-test overrides can replace any
// route (e.g. to delay, reorder or fail responses).

import { vi } from 'vitest'
import type { Campaign, Channel } from '@/api/types'
import {
  CAMPAIGN_ID,
  EXAMPLE_CONTENT_BRIEF,
  EXAMPLE_INTERPRETATION,
  EXAMPLE_RULE,
  audienceFor,
  draftCampaign,
  errorEnvelope,
} from './fixtures'

export interface RecordedCall {
  method: string
  path: string
  body: unknown
}

export interface FakeRequest {
  method: string
  path: string
  body: unknown
  signal: AbortSignal | null
}

export type Handler = (req: FakeRequest) => Response | Promise<Response> | undefined

export function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

export interface Deferred<T> {
  promise: Promise<T>
  resolve: (v: T) => void
}

export function deferred<T>(): Deferred<T> {
  let resolve!: (v: T) => void
  const promise = new Promise<T>((r) => (resolve = r))
  return { promise, resolve }
}

export class FakeBackend {
  campaigns = new Map<string, Campaign>()
  calls: RecordedCall[] = []
  overrides: Array<{ method: string; pattern: RegExp; handler: Handler }> = []
  nextId = CAMPAIGN_ID
  llmProvider = 'demo'
  /** Result of the next POST /interpret (default: the example interpretation). */
  interpretResult: (c: Campaign) => Response = (c) => this.defaultInterpret(c)

  constructor() {
    vi.stubGlobal('fetch', this.fetch)
  }

  seed(c: Campaign): Campaign {
    this.campaigns.set(c.id, structuredClone(c))
    return c
  }

  override(method: string, pattern: RegExp, handler: Handler): void {
    this.overrides.unshift({ method, pattern, handler })
  }

  callsTo(method: string, pattern: RegExp): RecordedCall[] {
    return this.calls.filter((c) => c.method === method && pattern.test(c.path))
  }

  fetch = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url
    const path = new URL(url, 'http://localhost').pathname
    const method = (init?.method ?? 'GET').toUpperCase()
    const body = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined
    const signal = init?.signal ?? null
    this.calls.push({ method, path, body })
    if (signal?.aborted) throw new DOMException('Aborted', 'AbortError')

    const req: FakeRequest = { method, path, body, signal }
    for (const o of this.overrides) {
      if (o.method === method && o.pattern.test(path)) {
        const res = await raceAbort(Promise.resolve(o.handler(req)), signal)
        if (res) return res
      }
    }
    return this.route(req)
  }

  private route(req: FakeRequest): Response {
    const { method, path, body } = req
    if (method === 'GET' && path === '/readyz') {
      return json(200, { status: 'ready', database: 'ok', migration_version: 1, migration_dirty: false, llm_provider: this.llmProvider })
    }
    if (method === 'POST' && path === '/api/v1/campaigns') {
      const b = body as { name?: string; brief?: string }
      const details = []
      if (!b.name?.trim()) details.push({ path: 'name', code: 'empty', message: 'must not be empty' })
      if ((b.brief?.trim().length ?? 0) < 10) details.push({ path: 'brief', code: 'too_short', message: 'must be at least 10 characters' })
      if (details.length) return json(422, errorEnvelope('validation_failed', 'request is invalid', { details }))
      const c = draftCampaign({ id: this.nextId, name: b.name!.trim(), brief: b.brief!.trim() })
      this.campaigns.set(c.id, c)
      return json(201, c)
    }
    const m = path.match(/^\/api\/v1\/campaigns\/([^/]+)(\/[a-z]+)?$/)
    if (!m) return json(404, errorEnvelope('not_found', 'resource not found'))
    const [, id, sub] = m
    const c = this.campaigns.get(id!)
    if (!c) return json(404, errorEnvelope('not_found', 'campaign not found'))

    if (method === 'GET' && !sub) return json(200, c)
    if (method === 'POST' && sub === '/interpret') return this.interpretResult(c)
    if (method === 'GET' && sub === '/audience') {
      if (c.status === 'draft') {
        return json(409, errorEnvelope('invalid_state', 'audience is not allowed while the campaign is draft: interpret the brief first'))
      }
      return json(200, {
        campaign_id: c.id,
        campaign_version: c.version,
        segment_version: c.segment_version,
        channels: c.channels,
        audience: audienceFor(c.channels),
      })
    }
    if (method === 'PUT' && sub === '/channels') {
      const b = body as { channels: Channel[]; expected_version: number }
      if (b.expected_version !== c.version) {
        return json(409, errorEnvelope('version_conflict', 'the campaign was changed by another request; reload it and try again'))
      }
      const chs = (['sms', 'email'] as Channel[]).filter((x) => b.channels.includes(x))
      const same = chs.length === c.channels.length && chs.every((x, i) => x === c.channels[i])
      const next = same ? c : { ...c, channels: chs, version: c.version + 1, updated_at: '2026-11-04T10:05:00Z' }
      this.campaigns.set(c.id, next)
      return json(200, next)
    }
    return json(405, errorEnvelope('method_not_allowed', 'method not allowed'))
  }

  /** Applies the example interpretation like campaign.WithInterpretation. */
  defaultInterpret(c: Campaign, opts: { suggested?: Channel[]; audienceError?: boolean } = {}): Response {
    const first = c.segment_version === 0 && c.channels.length === 0
    const next: Campaign = {
      ...c,
      status: 'segmented',
      segment_rule: structuredClone(EXAMPLE_RULE),
      segment_version: c.segment_version === 0 ? 1 : c.segment_version,
      interpretation: { ...EXAMPLE_INTERPRETATION },
      content_brief: { ...EXAMPLE_CONTENT_BRIEF },
      channels: first ? (opts.suggested ?? ['sms', 'email']) : c.channels,
      version: c.version + 1,
      updated_at: '2026-11-04T10:01:00Z',
    }
    this.campaigns.set(c.id, next)
    if (opts.audienceError) {
      return json(200, {
        campaign: next,
        audience: null,
        audience_error: {
          code: 'audience_unavailable',
          message: 'audience statistics are temporarily unavailable; try again',
          details: [],
          retryable: true,
        },
      })
    }
    return json(200, { campaign: next, audience: audienceFor(next.channels) })
  }

  /** Simulates a change made by another tab/request (version bump). */
  externalChange(id: string, patch: Partial<Campaign>): Campaign {
    const c = this.campaigns.get(id)!
    const next = { ...c, ...patch, version: c.version + 1 }
    this.campaigns.set(id, next)
    return next
  }
}

function raceAbort<T>(p: Promise<T>, signal: AbortSignal | null): Promise<T> {
  if (!signal) return p
  return new Promise<T>((resolve, reject) => {
    const onAbort = () => reject(new DOMException('Aborted', 'AbortError'))
    if (signal.aborted) return onAbort()
    signal.addEventListener('abort', onAbort, { once: true })
    p.then(
      (v) => {
        signal.removeEventListener('abort', onAbort)
        resolve(v)
      },
      (e) => {
        signal.removeEventListener('abort', onAbort)
        reject(e)
      },
    )
  })
}
