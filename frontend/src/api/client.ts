// Typed client for the CP6 API (base /api/v1). Every call:
//   - accepts an AbortSignal (callers abort superseded or unmounted work);
//   - decodes the response body at runtime (decode.ts);
//   - throws only ApiError, whose message is safe to display: it is either the
//     backend's fixed, display-safe envelope message or a client-authored text.
//     Raw response bodies (HTML error pages, proxy output, stack traces) are
//     never surfaced.

import {
  DecodeError,
  decodeAudienceResponse,
  decodeCampaign,
  decodeErrorBody,
  decodeInterpretResponse,
  decodeReadiness,
} from './decode'
import type {
  AudienceResponse,
  Campaign,
  Channel,
  CreateCampaignRequest,
  FieldError,
  InterpretResponse,
  ReadinessReport,
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

  if (!res.ok) {
    if (parsed && typeof payload === 'object' && payload !== null && 'error' in payload) {
      let body: ReturnType<typeof decodeErrorBody> | null = null
      try {
        body = decodeErrorBody((payload as { error: unknown }).error)
      } catch {
        body = null // not a valid envelope: generic error below
      }
      if (body) {
        throw new ApiError({
          kind: 'http',
          status: res.status,
          code: body.code,
          // Internal errors carry no actionable detail; keep the text generic.
          message: body.code === 'internal' ? genericHttpMessage(res.status) : body.message,
          details: body.details,
          retryable: body.retryable,
          rawProposal: body.raw_proposal,
        })
      }
    }
    throw new ApiError({
      kind: 'http',
      status: res.status,
      code: `http_${res.status}`,
      message: genericHttpMessage(res.status),
      retryable: res.status >= 500,
    })
  }

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

function campaignUrl(id: string): string {
  return `${API_BASE}/campaigns/${encodeURIComponent(id)}`
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

  /** GET /readyz → readiness incl. the LLM provider selected at startup. */
  readiness(signal?: AbortSignal): Promise<ReadinessReport> {
    return request('GET', '/readyz', decodeReadiness, { signal })
  },
}
