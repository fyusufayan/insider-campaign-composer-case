// The fetch-stream SSE client against the fake backend's controllable stream.
import { beforeEach, describe, expect, it } from 'vitest'
import { api, ApiError } from '@/api/client'
import type { GenerationEvent } from '@/api/types'
import { FakeBackend, json, type FakeSseStream } from './helpers/fakeBackend'
import { CAMPAIGN_ID, errorEnvelope, segmentedCampaign } from './helpers/fixtures'

let fake: FakeBackend
beforeEach(() => {
  fake = new FakeBackend()
  fake.seed(segmentedCampaign())
})

async function run(signal?: AbortSignal): Promise<{ events: GenerationEvent[]; result: unknown }> {
  const events: GenerationEvent[] = []
  try {
    const result = await api.generateContent(CAMPAIGN_ID, (e) => events.push(e), signal)
    return { events, result }
  } catch (e) {
    return { events, result: e }
  }
}

describe('generateContent stream client', () => {
  it('delivers events in order and resolves with the committed result', async () => {
    const { events, result } = await run()
    expect(events.map((e) => (e.type === 'status' ? `status:${e.data.phase}` : e.type))).toEqual([
      'meta',
      'status:generating',
      'delta',
      'delta',
      'delta',
      'delta',
      'delta',
      'delta',
      'status:validating',
      'status:persisting',
      'validated',
    ])
    expect(result).toMatchObject({ created: true, campaign: { status: 'content_ready', version: 3 } })
    const call = fake.callsTo('POST', /generations$/)[0]!
    expect(call.body).toEqual({})
  })

  it('rejects with the error event as an ApiError', async () => {
    fake.generation = (c, s) => {
      s.event('meta', { campaign_id: c.id, campaign_version: c.version, channels: c.channels, fields: ['sms_body'], prompt_version: 'content_v1', limits: { sms_body: 160, email_subject: 80, email_body: 5000 } })
      s.event('error', { status: 422, error: { code: 'ai_output_invalid', message: 'invalid after repair', details: [{ path: 'sms_body', code: 'unknown_variable', message: 'x' }], retryable: false, raw_proposal: '{"sms_body":"<b>x</b>"}' } })
      s.close()
    }
    const { result } = await run()
    expect(result).toBeInstanceOf(ApiError)
    const e = result as ApiError
    expect([e.kind, e.status, e.code, e.details.length, e.rawProposal]).toEqual(['http', 422, 'ai_output_invalid', 1, '{"sms_body":"<b>x</b>"}'])
  })

  it('maps a JSON error before the stream started', async () => {
    fake.override('POST', /generations$/, () => json(409, errorEnvelope('invalid_state', 'not now')))
    const { events, result } = await run()
    expect(events).toEqual([])
    expect(result).toMatchObject({ kind: 'http', status: 409, code: 'invalid_state', message: 'not now' })
  })

  it('reports a stream that ends without a terminal event as interrupted', async () => {
    fake.generation = (_c, s) => {
      s.event('status', { phase: 'generating', attempt: 1, reason: 'initial' })
      s.raw('event: validated\ndata: {"partial":') // cut mid-event
      s.close()
    }
    const { events, result } = await run()
    expect(events).toHaveLength(1)
    expect(result).toMatchObject({ kind: 'network', code: 'stream_interrupted', retryable: true })
  })

  it('rejects an event that violates the contract and stops reading', async () => {
    let stream!: FakeSseStream
    fake.generation = (_c, s) => {
      stream = s
      s.event('delta', { attempt: 1, field: 'sms_body', text: 'x', preview: false })
      // keep the stream open: the client must cancel it
    }
    const { result } = await run()
    expect(result).toMatchObject({ kind: 'invalid_response' })
    expect(stream.cancelled).toBe(true)
  })

  it('aborts when the caller aborts mid-stream', async () => {
    const ctrl = new AbortController()
    fake.generation = (_c, s) => {
      s.event('status', { phase: 'thinking', attempt: 1 })
    }
    const p = run(ctrl.signal)
    await new Promise((r) => setTimeout(r, 0))
    ctrl.abort()
    const { result } = await p
    expect(result).toMatchObject({ kind: 'aborted' })
    expect(fake.streams[0]!.closed).toBe(true)
  })
})
