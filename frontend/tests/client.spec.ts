import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api } from '@/api/client'
import { CAMPAIGN_ID, draftCampaign, errorEnvelope } from './helpers/fixtures'
import { json } from './helpers/fakeBackend'

function stubFetch(impl: (url: string, init?: RequestInit) => Promise<Response>) {
  const fn = vi.fn(impl)
  vi.stubGlobal('fetch', fn)
  return fn
}

async function rejection(p: Promise<unknown>): Promise<ApiError> {
  try {
    await p
  } catch (e) {
    expect(e).toBeInstanceOf(ApiError)
    return e as ApiError
  }
  throw new Error('expected a rejection')
}

afterEach(() => vi.unstubAllGlobals())

describe('api client', () => {
  it('sends the exact documented request shapes', async () => {
    const fetch = stubFetch(async () => json(200, draftCampaign({ status: 'segmented', version: 3 })))
    await api.setChannels(CAMPAIGN_ID, [], 2)
    const [url, init] = fetch.mock.calls[0]!
    expect(url).toBe(`/api/v1/campaigns/${CAMPAIGN_ID}/channels`)
    expect(init?.method).toBe('PUT')
    expect((init?.headers as Record<string, string>)['Content-Type']).toBe('application/json')
    expect(JSON.parse(init?.body as string)).toEqual({ channels: [], expected_version: 2 })

    await api.interpret(CAMPAIGN_ID).catch(() => undefined)
    const [iurl, iinit] = fetch.mock.calls[1]!
    expect(iurl).toBe(`/api/v1/campaigns/${CAMPAIGN_ID}/interpret`)
    expect(iinit?.method).toBe('POST')
    expect(iinit?.body).toBe('{}')
  })

  it('maps the error envelope including details, retryable and raw_proposal', async () => {
    stubFetch(async () =>
      json(
        422,
        errorEnvelope('ai_output_invalid', 'the AI proposal was invalid after one repair attempt; nothing was saved', {
          details: [{ path: 'segment.conditions[0].field', code: 'unknown_field', message: 'unknown field', allowed: ['city', 'age'] }],
          raw_proposal: '{"segment": <script>alert(1)</script>}',
        }),
      ),
    )
    const err = await rejection(api.interpret(CAMPAIGN_ID))
    expect(err.kind).toBe('http')
    expect(err.status).toBe(422)
    expect(err.code).toBe('ai_output_invalid')
    expect(err.details).toEqual([
      { path: 'segment.conditions[0].field', code: 'unknown_field', message: 'unknown field', allowed: ['city', 'age'] },
    ])
    expect(err.rawProposal).toBe('{"segment": <script>alert(1)</script>}')
  })

  it.each([
    [409, 'version_conflict', false],
    [409, 'invalid_state', false],
    [503, 'llm_unavailable', true],
    [503, 'audience_unavailable', true],
    [502, 'llm_truncated', false],
    [502, 'llm_rejected', false],
    [422, 'llm_refused', false],
    [422, 'invalid_brief', false],
  ])('maps %i %s', async (status, code, retryable) => {
    stubFetch(async () => json(status, errorEnvelope(code, 'server message', { retryable })))
    const err = await rejection(api.getCampaign(CAMPAIGN_ID))
    expect([err.status, err.code, err.retryable, err.message]).toEqual([status, code, retryable, 'server message'])
  })

  it('never surfaces a non-envelope error body (HTML, stack traces)', async () => {
    stubFetch(
      async () =>
        new Response('<html><body>panic: runtime error\ngoroutine 1 [running]: postgres://user:secret@db</body></html>', {
          status: 500,
          headers: { 'Content-Type': 'text/html' },
        }),
    )
    const err = await rejection(api.getCampaign(CAMPAIGN_ID))
    expect(err.code).toBe('http_500')
    expect(err.message).not.toMatch(/panic|postgres|secret|goroutine/)
  })

  it('keeps the internal error message generic', async () => {
    stubFetch(async () => json(500, errorEnvelope('internal', 'internal server error')))
    const err = await rejection(api.getCampaign(CAMPAIGN_ID))
    expect(err.code).toBe('internal')
    expect(err.message).toBe('The server could not complete the request.')
  })

  it('classifies network failures as retryable network errors', async () => {
    stubFetch(async () => {
      throw new TypeError('Failed to fetch')
    })
    const err = await rejection(api.getCampaign(CAMPAIGN_ID))
    expect(err.kind).toBe('network')
    expect(err.retryable).toBe(true)
  })

  it('classifies aborts', async () => {
    stubFetch(async (_url, init) => {
      if (init?.signal?.aborted) throw new DOMException('Aborted', 'AbortError')
      return json(200, draftCampaign())
    })
    const c = new AbortController()
    c.abort()
    const err = await rejection(api.getCampaign(CAMPAIGN_ID, c.signal))
    expect(err.kind).toBe('aborted')
    expect(err.isAbort).toBe(true)
  })

  it('rejects 2xx bodies that do not match the contract', async () => {
    stubFetch(async () => json(200, { id: CAMPAIGN_ID, status: 'weird' }))
    const err = await rejection(api.getCampaign(CAMPAIGN_ID))
    expect(err.kind).toBe('invalid_response')
  })

  it('decodes an interpret response carrying audience_error', async () => {
    stubFetch(async () =>
      json(200, {
        campaign: draftCampaign({ status: 'segmented', version: 2 }),
        audience: null,
        audience_error: { code: 'audience_unavailable', message: 'try again', details: [], retryable: true },
      }),
    )
    const res = await api.interpret(CAMPAIGN_ID)
    expect(res.audience).toBeNull()
    expect(res.audience_error?.code).toBe('audience_unavailable')
  })
})
