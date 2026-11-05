// A small stateful fake of the CP6 HTTP API, installed as global fetch.
// Tests exercise the real client, store and components against HTTP-shaped
// responses; only the network is faked. Per-test overrides can replace any
// route (e.g. to delay, reorder or fail responses).

import { vi } from 'vitest'
import type { ApprovalSummary, Campaign, Channel, ContentField, ContentVersion, DeliveryItem, DeliveryStats, FieldError } from '@/api/types'
import {
  APPROVAL_ID,
  approvalFor,
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
  deliveryStats,
  type ChannelInput,
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
  /** URL of the request being routed (query strings are not part of `path`). */
  lastUrl = ''
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
  /** Approvals by id (the fake keeps every approval, like the database). */
  approvals = new Map<string, ApprovalSummary>()
  nextApprovalSeq = 0
  pendingTtlMs = 15 * 60_000
  approvedTtlMs = 24 * 3_600_000
  /** Offer findings attached to the next prepared approval. */
  nextFindings: FieldError[] = []
  /** Delivery statistics per campaign (GET /stats); default derived from the campaign. */
  stats = new Map<string, DeliveryStats>()
  /** Delivery rows per campaign (GET /deliveries), ascending ids. */
  deliveries = new Map<string, DeliveryItem[]>()

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
    this.lastUrl = url
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
    const am = path.match(/^\/api\/v1\/campaigns\/([^/]+)\/approvals(?:\/([^/]+)(?:\/([a-z]+))?)?$/)
    if (am) return this.approvalRoute(method, am[1]!, am[2], am[3], body)
    const m = path.match(/^\/api\/v1\/campaigns\/([^/]+)(\/[a-z]+(?:\/[a-z]+)?)?$/)
    if (!m) return json(404, errorEnvelope('not_found', 'resource not found'))
    const [, id, sub] = m
    const c = this.campaigns.get(id!)
    if (!c) return json(404, errorEnvelope('not_found', 'campaign not found'))

    if (method === 'GET' && !sub) {
      this.lazyExpire(c.id)
      return json(200, this.campaigns.get(c.id))
    }
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
    if (method === 'POST' && sub === '/send') return this.send(c, body as Record<string, unknown>)
    if (method === 'POST' && sub === '/cancel') return this.cancel(c, body as Record<string, unknown>)
    if (method === 'GET' && sub === '/stats') return json(200, this.statsOf(c))
    if (method === 'GET' && sub === '/deliveries') return this.listDeliveries(c, req)
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

  // ---- approvals (CP15) ----

  private approvalResponse(status: number, c: Campaign, a: ApprovalSummary): Response {
    return json(status, { campaign: c, approval: a, server_time: new Date().toISOString() })
  }

  /** Expires the campaign's live approval if due (like approval.ExpireDue): version + 1. */
  lazyExpire(id: string): string | null {
    const c = this.campaigns.get(id)
    if (!c?.active_approval_id || (c.status !== 'pending_approval' && c.status !== 'approved')) return null
    const a = this.approvals.get(c.active_approval_id)!
    if (Date.parse(a.expires_at) > Date.now()) return null
    const next: Campaign = { ...c, status: 'content_ready', active_approval_id: null, version: c.version + 1 }
    this.campaigns.set(id, next)
    this.approvals.set(a.approval_id, this.closed(a, next, { status: 'expired' }))
    return a.approval_id
  }

  private closed(a: ApprovalSummary, c: Campaign, patch: Partial<ApprovalSummary>): ApprovalSummary {
    const snapshot: ApprovalSummary['snapshot'] = {}
    for (const ch of a.channels) snapshot[ch] = { recipients: a.snapshot[ch]!.recipients, render_issues: null }
    return { ...a, ...patch, campaign_version: c.version, snapshot, review: null, rendered_content: null, closed_at: new Date().toISOString() }
  }

  /** Puts a campaign into pending_approval with an approval (test setup). */
  seedPending(c: Campaign, v: ContentVersion, overrides: Partial<ApprovalSummary> = {}): { campaign: Campaign; approval: ApprovalSummary } {
    const list = this.contents.get(c.id) ?? []
    if (!list.some((x) => x.id === v.id)) this.contents.set(c.id, [...list, v])
    const campaign: Campaign = { ...c, status: 'pending_approval', current_content_id: v.id, active_approval_id: overrides.approval_id ?? APPROVAL_ID }
    this.campaigns.set(c.id, campaign)
    const now = Date.now()
    const a = approvalFor(campaign, v, {
      prepared_at: new Date(now).toISOString(),
      expires_at: new Date(now + this.pendingTtlMs).toISOString(),
      ...overrides,
    })
    this.approvals.set(a.approval_id, a)
    return { campaign, approval: a }
  }

  private approvalRoute(method: string, id: string, aid: string | undefined, action: string | undefined, body: unknown): Response {
    const conflict = () => json(409, errorEnvelope('version_conflict', 'the campaign was changed by another request; reload it and try again'))
    if (!this.campaigns.has(id)) return json(404, errorEnvelope('not_found', 'campaign not found'))
    const expiredNow = this.lazyExpire(id)
    const c = this.campaigns.get(id)!
    const b = (body ?? {}) as Record<string, unknown>

    if (method === 'POST' && !aid) {
      if (b.expected_version !== c.version) return conflict()
      if (c.status !== 'content_ready') {
        return json(409, errorEnvelope('invalid_state', `prepare_approval is not allowed while the campaign is ${c.status}`))
      }
      const v = this.current(c)!
      const approvalId = this.nextApprovalSeq++ === 0 ? APPROVAL_ID : `a1b2c3d4-e5f6-4a7b-8c9d-${String(this.nextApprovalSeq).padStart(12, '0')}`
      const next: Campaign = { ...c, status: 'pending_approval', active_approval_id: approvalId, version: c.version + 1 }
      this.campaigns.set(id, next)
      const now = Date.now()
      const a = approvalFor(next, v, {
        approval_id: approvalId,
        prepared_at: new Date(now).toISOString(),
        expires_at: new Date(now + this.pendingTtlMs).toISOString(),
      })
      a.review!.offer_findings = this.nextFindings
      this.approvals.set(approvalId, a)
      return this.approvalResponse(201, next, a)
    }
    const a = aid ? this.approvals.get(aid) : undefined
    if (!a || a.campaign_id !== id) return json(404, errorEnvelope('not_found', 'approval not found'))
    if (method === 'GET' && !action) {
      return this.approvalResponse(200, c, { ...a, campaign_version: c.version })
    }
    if (method !== 'POST' || (action !== 'confirm' && action !== 'withdraw')) {
      return json(405, errorEnvelope('method_not_allowed', 'method not allowed'))
    }
    if (a.status === 'expired' || expiredNow === a.approval_id) {
      return json(409, errorEnvelope('approval_expired', 'the approval has expired; its recipient snapshot was released — reload the campaign and prepare a new approval'))
    }
    if (action === 'confirm') {
      if (a.status !== 'pending') return json(409, errorEnvelope('invalid_state', 'confirm_approval is not allowed: the approval is already confirmed'))
      if (b.expected_version !== c.version) return conflict()
      const chs = b.channels as Channel[]
      if (b.segment_version !== a.segment_version || b.content_version !== a.content_version || chs.join() !== a.channels.join()) {
        return json(409, errorEnvelope('approval_mismatch', 'the approval no longer matches what was reviewed', { details: [{ path: 'content_version', code: 'approval_mismatch', message: 'differs from the prepared approval' }] }))
      }
      const next: Campaign = { ...c, status: 'approved', version: c.version + 1 }
      this.campaigns.set(id, next)
      const now = Date.now()
      const approved: ApprovalSummary = {
        ...a,
        status: 'approved',
        campaign_version: next.version,
        approved_at: new Date(now).toISOString(),
        approved_by: 'marketer@demo',
        expires_at: new Date(now + this.approvedTtlMs).toISOString(),
      }
      this.approvals.set(a.approval_id, approved)
      return this.approvalResponse(200, next, approved)
    }
    // withdraw
    if (a.status === 'revoked' || a.status === 'consumed') {
      return json(409, errorEnvelope('invalid_state', 'withdraw_approval is not allowed: the approval was already withdrawn or replaced'))
    }
    if (b.expected_version !== c.version) return conflict()
    const next: Campaign = { ...c, status: 'content_ready', active_approval_id: null, version: c.version + 1 }
    this.campaigns.set(id, next)
    const w = this.closed(a, next, { status: 'revoked', revoke_reason: 'withdrawn' })
    this.approvals.set(a.approval_id, w)
    return this.approvalResponse(200, next, w)
  }

  // ---- send / cancel / stats (CP17B) ----

  statsOf(c: Campaign): DeliveryStats {
    const cur = this.campaigns.get(c.id)!
    const st = this.stats.get(c.id)
    if (st) return { ...st, campaign_status: cur.status, campaign_version: cur.version }
    return deliveryStats(cur)
  }

  private send(c: Campaign, b: Record<string, unknown>): Response {
    this.lazyExpire(c.id)
    c = this.campaigns.get(c.id)!
    const a = this.approvals.get(String(b.approval_id))
    if (!a || a.campaign_id !== c.id) return json(404, errorEnvelope('not_found', 'approval not found'))
    const res = (status: number, next: Campaign, started: boolean) => {
      const released: Partial<Record<Channel, number>> = {}
      for (const ch of a.channels) released[ch] = a.snapshot[ch]!.recipients
      return json(status, { status: next.status, approval_id: a.approval_id, released, already_started: !started, campaign: next, server_time: new Date().toISOString() })
    }
    if (a.status === 'consumed' && c.active_approval_id === a.approval_id && ['sending', 'completed', 'completed_with_failures'].includes(c.status)) {
      return res(200, c, false)
    }
    if (a.status === 'expired') return json(409, errorEnvelope('approval_expired', 'the approval has expired'))
    if (c.status !== 'approved' || a.status !== 'approved' || c.active_approval_id !== a.approval_id) {
      return json(409, errorEnvelope('invalid_state', `send is not allowed while the campaign is ${c.status}`))
    }
    if (b.expected_version !== c.version) return json(409, errorEnvelope('version_conflict', 'the campaign was changed by another request; reload it and try again'))
    const next: Campaign = { ...c, status: 'sending', version: c.version + 1 }
    this.campaigns.set(c.id, next)
    this.approvals.set(a.approval_id, { ...a, status: 'consumed', campaign_version: next.version })
    const chs: Partial<Record<Channel, { pending: number }>> = {}
    for (const ch of a.channels) chs[ch] = { pending: a.snapshot[ch]!.recipients }
    this.stats.set(c.id, deliveryStats(next, { channels: chs, approvalId: a.approval_id }))
    return res(202, next, true)
  }

  private cancel(c: Campaign, b: Record<string, unknown>): Response {
    if (['cancelled', 'completed', 'completed_with_failures'].includes(c.status)) {
      return json(409, errorEnvelope('invalid_state', `cancel is not allowed while the campaign is ${c.status}`))
    }
    if (b.expected_version !== c.version) return json(409, errorEnvelope('version_conflict', 'the campaign was changed by another request; reload it and try again'))
    const next: Campaign = { ...c, status: 'cancelled', version: c.version + 1 }
    this.campaigns.set(c.id, next)
    const cancelled: Partial<Record<Channel, number>> = {}
    const inFlight: Partial<Record<Channel, number>> = {}
    const st = this.stats.get(c.id)
    if (st && c.status === 'sending') {
      const input: Partial<Record<Channel, ChannelInput>> = {}
      for (const ch of ['sms', 'email'] as Channel[]) {
        const x = st.channels[ch]
        if (!x) continue
        cancelled[ch] = x.pending + x.retry_scheduled
        inFlight[ch] = x.in_flight
        input[ch] = {
          in_flight: x.in_flight,
          sent: x.sent,
          failed: x.failed,
          skipped_consent_revoked: x.skipped_consent_revoked,
          skipped_no_contact: x.skipped_no_contact,
          cancelled: x.cancelled + x.pending + x.retry_scheduled,
          uncertain: x.uncertain,
          attempts: x.attempts.by_outcome,
          reasons: x.reasons,
        }
      }
      this.stats.set(c.id, deliveryStats(next, { channels: input, approvalId: st.approval_id ?? APPROVAL_ID }))
    }
    return json(200, {
      status: 'cancelled',
      previous_status: c.status,
      revoked_approval_id: null,
      released_held_rows: 0,
      cancelled_deliveries: cancelled,
      in_flight: inFlight,
      campaign: next,
      server_time: new Date().toISOString(),
    })
  }

  private listDeliveries(c: Campaign, req: FakeRequest): Response {
    const url = new URL(this.lastUrl, 'http://localhost')
    const q = url.searchParams
    const limit = Number(q.get('limit') ?? 50)
    const after = Number(q.get('cursor') ?? 0)
    const status = q.get('status')
    const uncertain = q.get('uncertain')
    void req
    const all = (this.deliveries.get(c.id) ?? []).filter(
      (d) => d.id > after && (!status || d.status === status) && (uncertain === null || String(d.outcome_uncertain) === uncertain),
    )
    const items = all.slice(0, limit)
    const st = this.stats.get(c.id)
    return json(200, {
      campaign_id: c.id,
      approval_id: st?.approval_id ?? null,
      items,
      next_cursor: all.length > limit ? String(items[items.length - 1]!.id) : null,
      limit,
    })
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
