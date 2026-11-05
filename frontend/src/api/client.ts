// Typed client for the CP6 API (base /api/v1). Every call:
//   - accepts an AbortSignal (callers abort superseded or unmounted work);
//   - decodes the response body at runtime (decode.ts);
//   - throws only ApiError, whose message is safe to display: it is either the
//     backend's fixed, display-safe envelope message or a client-authored text.
//     Raw response bodies (HTML error pages, proxy output, stack traces) are
//     never surfaced.

import {
  DecodeError,
  decodeApprovalResponse,
  decodeAudienceResponse,
  decodeCampaign,
  decodeContentSaved,
  decodeCurrentContent,
  decodeDraftValidation,
  decodeErrorBody,
  decodeGenerationEvent,
  decodeInterpretResponse,
  decodeReadiness,
} from './decode'
import { SseLimitError, SseParser } from './sse'
import type {
  ApprovalResponse,
  ConfirmApprovalRequest,
  PrepareApprovalRequest,
  WithdrawApprovalRequest,
  AudienceResponse,
  Campaign,
  Channel,
  ContentDraft,
  ContentSaved,
  CreateCampaignRequest,
  CurrentContent,
  DraftValidation,
  ErrorBody,
  FieldError,
  GenerationEvent,
  InterpretResponse,
  ReadinessReport,
  SaveContentRequest,
  SetChannelsRequest,
} from './types'

export const API_BASE = '/api/v1'

/**
 * - http: the server answered with a non-2xx status (code from the envelope,
 *   or `http_<status>` when the body was not a valid envelope);
 * - network: no response (server down, proxy refused, offline);
 * - aborted: the caller aborted the request (never shown to the user);
 * - invalid_response: a 2xx body that does not match the contract.
 */
export type ApiErrorKind = 'http' | 'network' | 'aborted' | 'invalid_response'

export class ApiError extends Error {
  readonly kind: ApiErrorKind
  readonly status: number
  readonly code: string
  readonly details: FieldError[]
  readonly retryable: boolean
  /** Untrusted model output (ai_output_invalid only). Render escaped. */
  readonly rawProposal: string | undefined

  constructor(init: {
    kind: ApiErrorKind
    status?: number
    code: string
    message: string
    details?: FieldError[]
    retryable?: boolean
    rawProposal?: string | undefined
  }) {
    super(init.message)
    this.name = 'ApiError'
    this.kind = init.kind
    this.status = init.status ?? 0
    this.code = init.code
    this.details = init.details ?? []
    this.retryable = init.retryable ?? false
    this.rawProposal = init.rawProposal
  }

  get isAbort(): boolean {
    return this.kind === 'aborted'
  }
}

export function isApiError(e: unknown): e is ApiError {
  return e instanceof ApiError
}

function genericHttpMessage(status: number): string {
  if (status === 404) return 'The requested resource was not found.'
  if (status === 502 || status === 503 || status === 504) {
    return 'The server is temporarily unavailable. Try again in a moment.'
  }
  if (status >= 500) return 'The server could not complete the request.'
  return `The request failed (HTTP ${status}).`
}

function apiErrorFromBody(status: number, body: ErrorBody): ApiError {
  return new ApiError({
    kind: 'http',
    status,
    code: body.code,
    // Internal errors carry no actionable detail; keep the text generic.
    message: body.code === 'internal' ? genericHttpMessage(status) : body.message,
    details: body.details,
    retryable: body.retryable,
    rawProposal: body.raw_proposal,
  })
}

/** The ApiError of a non-2xx response (payload: parsed JSON, if any). */
function httpError(status: number, payload: unknown): ApiError {
  if (typeof payload === 'object' && payload !== null && 'error' in payload) {
    try {
      return apiErrorFromBody(status, decodeErrorBody((payload as { error: unknown }).error))
    } catch {
      // not a valid envelope: generic error below
    }
  }
  return new ApiError({
    kind: 'http',
    status,
    code: `http_${status}`,
    message: genericHttpMessage(status),
    retryable: status >= 500,
  })
}

function aborted(): ApiError {
  return new ApiError({ kind: 'aborted', code: 'aborted', message: 'The request was cancelled.' })
}

function isAbortError(e: unknown, signal?: AbortSignal): boolean {
  return !!signal?.aborted || (e instanceof DOMException && e.name === 'AbortError')
}

interface RequestOptions {
  body?: unknown
  signal?: AbortSignal | undefined
}

async function request<T>(
  method: 'GET' | 'POST' | 'PUT',
  url: string,
  decode: (v: unknown) => T,
  opts: RequestOptions = {},
): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  const init: RequestInit = { method, headers, signal: opts.signal ?? null }
  if (opts.body !== undefined) {
    headers['Content-Type'] = 'application/json'
    init.body = JSON.stringify(opts.body)
  }

  let res: Response
  try {
    res = await fetch(url, init)
  } catch (e) {
    if (opts.signal?.aborted || (e instanceof DOMException && e.name === 'AbortError')) {
      throw new ApiError({ kind: 'aborted', code: 'aborted', message: 'The request was cancelled.' })
    }
    throw new ApiError({
      kind: 'network',
      code: 'network_error',
      message: 'Cannot reach the server. Check that the backend is running and try again.',
      retryable: true,
    })
  }

  let payload: unknown = undefined
  let parsed = false
  try {
    const text = await res.text()
    if (text.trim() !== '') {
      payload = JSON.parse(text)
      parsed = true
    }
  } catch (e) {
    if (opts.signal?.aborted || (e instanceof DOMException && e.name === 'AbortError')) {
      throw new ApiError({ kind: 'aborted', code: 'aborted', message: 'The request was cancelled.' })
    }
    parsed = false
  }
  if (opts.signal?.aborted) {
    throw new ApiError({ kind: 'aborted', code: 'aborted', message: 'The request was cancelled.' })
  }

  if (!res.ok) throw httpError(res.status, parsed ? payload : undefined)

  try {
    return decode(payload)
  } catch (e) {
    if (e instanceof DecodeError && import.meta.env.DEV) {
      console.warn('API response did not match the contract:', e.message)
    }
    throw new ApiError({
      kind: 'invalid_response',
      status: res.status,
      code: 'invalid_response',
      message: 'The server sent an unexpected response.',
    })
  }
}

/**
 * Reads the generation stream (POST returning text/event-stream). Every event
 * is decoded strictly and handed to onEvent in order; the promise resolves
 * with the "validated" payload (sent only after the content was committed)
 * and rejects with an ApiError for the "error" event, a non-2xx JSON error
 * response (request errors before the stream started), an aborted request,
 * a stream that ends without a terminal event (kind network,
 * code stream_interrupted) or an event that violates the contract
 * (invalid_response; the stream is cancelled).
 */
async function generationStream(
  url: string,
  signal: AbortSignal | undefined,
  onEvent: (ev: GenerationEvent) => void,
): Promise<ContentSaved> {
  let res: Response
  try {
    res = await fetch(url, {
      method: 'POST',
      headers: { Accept: 'text/event-stream', 'Content-Type': 'application/json' },
      body: '{}',
      signal: signal ?? null,
    })
  } catch (e) {
    if (isAbortError(e, signal)) throw aborted()
    throw new ApiError({
      kind: 'network',
      code: 'network_error',
      message: 'Cannot reach the server. Check that the backend is running and try again.',
      retryable: true,
    })
  }

  const type = res.headers.get('Content-Type') ?? ''
  if (!res.ok || !type.startsWith('text/event-stream')) {
    let payload: unknown = undefined
    try {
      const text = await res.text()
      payload = text.trim() === '' ? undefined : JSON.parse(text)
    } catch (e) {
      if (isAbortError(e, signal)) throw aborted()
    }
    if (signal?.aborted) throw aborted()
    if (!res.ok) throw httpError(res.status, payload)
    throw new ApiError({ kind: 'invalid_response', status: res.status, code: 'invalid_response', message: 'The server sent an unexpected response.' })
  }
  if (!res.body) {
    throw new ApiError({ kind: 'invalid_response', status: res.status, code: 'invalid_response', message: 'The server sent an unexpected response.' })
  }

  const reader = res.body.getReader()
  // Mutated by the parser callback (an object, so TypeScript does not narrow it).
  const st: { terminal: ContentSaved | ApiError | null; violation: boolean } = { terminal: null, violation: false }
  const parser = new SseParser((m) => {
    if (st.terminal || st.violation) return
    let ev: GenerationEvent
    try {
      ev = decodeGenerationEvent(m.event, JSON.parse(m.data))
    } catch (e) {
      if (import.meta.env.DEV) console.warn('generation event did not match the contract:', (e as Error).message)
      st.violation = true
      return
    }
    onEvent(ev)
    if (ev.type === 'validated') st.terminal = ev.data
    else if (ev.type === 'error') st.terminal = apiErrorFromBody(ev.data.status, ev.data.error)
  })

  const interrupted = () =>
    new ApiError({
      kind: 'network',
      code: 'stream_interrupted',
      message: 'The connection was lost during generation.',
      retryable: true,
    })
  const invalid = () =>
    new ApiError({ kind: 'invalid_response', status: res.status, code: 'invalid_response', message: 'The server sent an unexpected response.' })

  try {
    while (!st.terminal && !st.violation) {
      const { done, value } = await reader.read()
      if (done) break
      parser.pushBytes(value)
    }
    if (!st.terminal && !st.violation) parser.end()
  } catch (e) {
    if (isAbortError(e, signal)) throw aborted()
    if (!st.terminal) {
      if (e instanceof SseLimitError) st.violation = true
      else throw interrupted()
    }
  } finally {
    // Stop reading; the server sees the disconnect if it is still sending.
    void reader.cancel().catch(() => {})
  }
  const terminal = st.terminal
  if (terminal instanceof ApiError) throw terminal
  if (terminal) return terminal
  if (signal?.aborted) throw aborted()
  if (st.violation) throw invalid()
  throw interrupted()
}

function campaignUrl(id: string): string {
  return `${API_BASE}/campaigns/${encodeURIComponent(id)}`
}

function approvalUrl(id: string, approvalId: string): string {
  return `${campaignUrl(id)}/approvals/${encodeURIComponent(approvalId)}`
}

export const api = {
  /** POST /api/v1/campaigns → 201 campaign. */
  createCampaign(req: CreateCampaignRequest, signal?: AbortSignal): Promise<Campaign> {
    return request('POST', `${API_BASE}/campaigns`, (v) => decodeCampaign(v), { body: req, signal })
  },

  /** GET /api/v1/campaigns/{id} → 200 campaign. */
  getCampaign(id: string, signal?: AbortSignal): Promise<Campaign> {
    return request('GET', campaignUrl(id), (v) => decodeCampaign(v), { signal })
  },

  /** POST /api/v1/campaigns/{id}/interpret (body {}) → 200 {campaign, audience, audience_error?}. */
  interpret(id: string, signal?: AbortSignal): Promise<InterpretResponse> {
    return request('POST', `${campaignUrl(id)}/interpret`, decodeInterpretResponse, { body: {}, signal })
  },

  /** GET /api/v1/campaigns/{id}/audience → 200 live preview. */
  getAudience(id: string, signal?: AbortSignal): Promise<AudienceResponse> {
    return request('GET', `${campaignUrl(id)}/audience`, decodeAudienceResponse, { signal })
  },

  /** PUT /api/v1/campaigns/{id}/channels {channels, expected_version} → 200 campaign. */
  setChannels(id: string, channels: Channel[], expectedVersion: number, signal?: AbortSignal): Promise<Campaign> {
    const body: SetChannelsRequest = { channels, expected_version: expectedVersion }
    return request('PUT', `${campaignUrl(id)}/channels`, (v) => decodeCampaign(v), { body, signal })
  },

  /** GET /api/v1/campaigns/{id}/content → current content (content: null without one). */
  getContent(id: string, signal?: AbortSignal): Promise<CurrentContent> {
    return request('GET', `${campaignUrl(id)}/content`, (v) => decodeCurrentContent(v), { signal })
  },

  /** POST /api/v1/campaigns/{id}/content/validate → read-only validation of a draft. */
  validateContent(id: string, draft: ContentDraft, signal?: AbortSignal): Promise<DraftValidation> {
    return request('POST', `${campaignUrl(id)}/content/validate`, decodeDraftValidation, { body: draft, signal })
  },

  /** PUT /api/v1/campaigns/{id}/content → {campaign, content, created}. */
  saveContent(id: string, draft: ContentDraft, expectedVersion: number, signal?: AbortSignal): Promise<ContentSaved> {
    const body: SaveContentRequest = { ...draft, expected_version: expectedVersion }
    return request('PUT', `${campaignUrl(id)}/content`, (v) => decodeContentSaved(v), { body, signal })
  },

  /** POST /api/v1/campaigns/{id}/content/generations (SSE). Resolves after the commit. */
  generateContent(id: string, onEvent: (ev: GenerationEvent) => void, signal?: AbortSignal): Promise<ContentSaved> {
    return generationStream(`${campaignUrl(id)}/content/generations`, signal, onEvent)
  },

  /** POST /api/v1/campaigns/{id}/approvals {expected_version} → 201 {campaign, approval}. */
  prepareApproval(id: string, expectedVersion: number, signal?: AbortSignal): Promise<ApprovalResponse> {
    const body: PrepareApprovalRequest = { expected_version: expectedVersion }
    return request('POST', `${campaignUrl(id)}/approvals`, (v) => decodeApprovalResponse(v), { body, signal })
  },

  /** GET /api/v1/campaigns/{id}/approvals/{aid} → {campaign, approval} (lazy expiry first). */
  getApproval(id: string, approvalId: string, signal?: AbortSignal): Promise<ApprovalResponse> {
    return request('GET', approvalUrl(id, approvalId), (v) => decodeApprovalResponse(v), { signal })
  },

  /** POST …/approvals/{aid}/confirm {expected_version, segment_version, content_version, channels}. */
  confirmApproval(id: string, approvalId: string, req: ConfirmApprovalRequest, signal?: AbortSignal): Promise<ApprovalResponse> {
    return request('POST', `${approvalUrl(id, approvalId)}/confirm`, (v) => decodeApprovalResponse(v), { body: req, signal })
  },

  /** POST …/approvals/{aid}/withdraw {expected_version}. */
  withdrawApproval(id: string, approvalId: string, expectedVersion: number, signal?: AbortSignal): Promise<ApprovalResponse> {
    const body: WithdrawApprovalRequest = { expected_version: expectedVersion }
    return request('POST', `${approvalUrl(id, approvalId)}/withdraw`, (v) => decodeApprovalResponse(v), { body, signal })
  },

  /** GET /readyz → readiness incl. the LLM provider selected at startup. */
  readiness(signal?: AbortSignal): Promise<ReadinessReport> {
    return request('GET', '/readyz', decodeReadiness, { signal })
  },
}
