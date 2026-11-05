// A small stateful fake of the CP6 HTTP API, installed as global fetch.
// Tests exercise the real client, store and components against HTTP-shaped
// responses; only the network is faked. Per-test overrides can replace any
// route (e.g. to delay, reorder or fail responses).

import { vi } from 'vitest'
import type { Campaign, Channel, ContentField, ContentVersion } from '@/api/types'
import {
  CAMPAIGN_ID,
  CONTENT_LIMITS,
  EXAMPLE_CONTENT_BRIEF,
  EXAMPLE_INTERPRETATION,
  EXAMPLE_RULE,
  EXAMPLE_VALUES,
  GEN_BODY,
  GEN_SMS,
  GEN_SUBJECT,
  audienceFor,
  currentContent,
  draftCampaign,
  errorEnvelope,
  fakeValidate,
} from './fixtures'

const encoder = new TextEncoder()

/**
 * A server-sent event response body that a test writes incrementally. An
 * abort of the request errors the stream like a real fetch would.
 */
export class FakeSseStream {
  readonly readable: ReadableStream<Uint8Array>
  private ctrl!: ReadableStreamDefaultController<Uint8Array>
  closed = false
  /** The client cancelled reading (e.g. after the terminal event or an abort). */
  cancelled = false

  constructor(signal: AbortSignal | null) {
    this.readable = new ReadableStream<Uint8Array>({
      start: (c) => {
        this.ctrl = c
      },
      cancel: () => {
        this.cancelled = true
        this.closed = true
      },
    })
    signal?.addEventListener('abort', () => {
      if (this.closed) return
      this.closed = true
      this.ctrl.error(new DOMException('Aborted', 'AbortError'))
    })
  }

  raw(text: string): void {
    if (!this.closed) this.ctrl.enqueue(encoder.encode(text))
  }

  event(name: string, data: unknown): void {
    this.raw(`event: ${name}\ndata: ${JSON.stringify(data)}\n\n`)
  }

  close(): void {
    if (this.closed) return
    this.closed = true
    this.ctrl.close()
  }
}

export const DEMO_NOTICE = 'Demo content generator (fixed templates, not Claude) produced this content.'

export type GenerationScript = (c: Campaign, stream: FakeSseStream, fake: FakeBackend) => void | Promise<void>

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

export type Handler = (req: FakeRequest) => Response | Promise<Response | undefined> | undefined

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
  /** Content versions per campaign (immutable, in order). */
  contents = new Map<string, ContentVersion[]>()
  nextContentId = 101
  /** Writes the generation stream (default: a successful demo generation). */
  generation: GenerationScript = (c, s) => this.defaultGeneration(c, s)
  streams: FakeSseStream[] = []

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
    const m = path.match(/^\/api\/v1\/campaigns\/([^/]+)(\/[a-z]+(?:\/[a-z]+)?)?$/)
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
    if (method === 'GET' && sub === '/content') return json(200, currentContent(c, this.current(c)))
    if (method === 'POST' && sub === '/content/validate') {
      if (c.status === 'draft') return json(409, errorEnvelope('invalid_state', 'validate_content is not allowed while the campaign is draft'))
      return json(200, {
        campaign_id: c.id,
        campaign_version: c.version,
        channels: c.channels,
        validation: fakeValidate(body as Record<ContentField, string | null>, c.channels),
        example_values: { ...EXAMPLE_VALUES },
        limits: { ...CONTENT_LIMITS },
      })
    }
    if (method === 'PUT' && sub === '/content') return this.saveContent(c, body as Record<string, unknown>)
    if (method === 'POST' && sub === '/content/generations') {
      if (c.status !== 'segmented' && c.status !== 'content_ready') {
        return json(409, errorEnvelope('invalid_state', `generate_content is not allowed while the campaign is ${c.status}`))
      }
      const stream = new FakeSseStream(req.signal)
      this.streams.push(stream)
      void Promise.resolve(this.generation(c, stream, this))
      return new Response(stream.readable, { status: 200, headers: { 'Content-Type': 'text/event-stream; charset=utf-8' } })
    }
    return json(405, errorEnvelope('method_not_allowed', 'method not allowed'))
  }

  current(c: Campaign): ContentVersion | null {
    if (c.current_content_id === null) return null
    return this.contents.get(c.id)?.find((v) => v.id === c.current_content_id) ?? null
  }

  /** Stores a new version and points the campaign at it (like SaveContent). */
  commitVersion(c: Campaign, v: Omit<ContentVersion, 'id' | 'version' | 'created_at'>): { campaign: Campaign; version: ContentVersion } {
    const list = this.contents.get(c.id) ?? []
    const version: ContentVersion = {
      ...v,
      id: this.nextContentId++,
      version: list.length + 1,
      created_at: `2026-11-04T10:${String(10 + list.length).padStart(2, '0')}:00Z`,
    }
    this.contents.set(c.id, [...list, version])
    const next: Campaign = {
      ...c,
      status: 'content_ready',
      current_content_id: version.id,
      version: c.version + 1,
      updated_at: version.created_at,
    }
    this.campaigns.set(c.id, next)
    return { campaign: next, version }
  }

  savedPayload(c: Campaign, v: ContentVersion, created: boolean, notes: string[] = []) {
    return { campaign: c, content: currentContent(c, v), created, notes }
  }

  private saveContent(c: Campaign, b: Record<string, unknown>): Response {
    if (b.expected_version !== c.version) {
      return json(409, errorEnvelope('version_conflict', 'the campaign was changed by another request; reload it and try again'))
    }
    if (c.status !== 'segmented' && c.status !== 'content_ready') {
      return json(409, errorEnvelope('invalid_state', `edit_content is not allowed while the campaign is ${c.status}`))
    }
    const draft = {
      sms_body: (b.sms_body as string | null) ?? null,
      email_subject: (b.email_subject as string | null) ?? null,
      email_body: (b.email_body as string | null) ?? null,
    }
    const res = fakeValidate(draft, c.channels)
    if (!res.valid) {
      return json(422, errorEnvelope('validation_failed', 'the content is invalid; nothing was saved', { details: res.errors }))
    }
    const canon = (s: string | null) => (s === null ? null : s.normalize('NFC').trim())
    const cur = this.current(c)
    const text = { sms_body: canon(draft.sms_body), email_subject: canon(draft.email_subject), email_body: canon(draft.email_body) }
    if (cur && cur.sms_body === text.sms_body && cur.email_subject === text.email_subject && cur.email_body === text.email_body) {
      return json(200, this.savedPayload(c, cur, false))
    }
    const { campaign, version } = this.commitVersion(c, { origin: 'edited', model: null, prompt_version: null, ...text })
    return json(200, this.savedPayload(campaign, version, true))
  }

  /** A successful demo generation streamed in small pieces. */
  defaultGeneration(c: Campaign, s: FakeSseStream): void {
    const fields: ContentField[] = []
    if (c.channels.includes('sms')) fields.push('sms_body')
    if (c.channels.includes('email')) fields.push('email_subject', 'email_body')
    const text: Record<ContentField, string> = { sms_body: GEN_SMS, email_subject: GEN_SUBJECT, email_body: GEN_BODY }
    s.event('meta', { campaign_id: c.id, campaign_version: c.version, channels: c.channels, fields, prompt_version: 'content_v1', limits: CONTENT_LIMITS })
    s.event('status', { phase: 'generating', attempt: 1, reason: 'initial' })
    for (const f of fields) {
      const t = text[f]
      s.event('delta', { attempt: 1, field: f, text: t.slice(0, 10), preview: true })
      s.event('delta', { attempt: 1, field: f, text: t.slice(10), preview: true })
    }
    s.event('status', { phase: 'validating', attempt: 1 })
    s.event('status', { phase: 'persisting' })
    const { campaign, version } = this.commitVersion(c, {
      origin: 'ai',
      model: 'demo',
      prompt_version: 'content_v1',
      sms_body: fields.includes('sms_body') ? GEN_SMS : null,
      email_subject: fields.includes('email_subject') ? GEN_SUBJECT : null,
      email_body: fields.includes('email_body') ? GEN_BODY : null,
    })
    s.event('validated', this.savedPayload(campaign, version, true, [DEMO_NOTICE]))
    s.close()
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
