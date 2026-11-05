// Strict runtime decoding of the CP17B delivery DTOs: statistics, stream
// events, the delivery list and the send/cancel responses. A payload that
// does not reconcile, carries unknown (possibly sensitive) properties or an
// unmasked destination is rejected as a whole.
import { describe, expect, it } from 'vitest'
import {
  DecodeError,
  decodeCancelResponse,
  decodeDeliveryEvent,
  decodeDeliveryPage,
  decodeDeliveryStats,
  decodeReadiness,
  decodeSendResponse,
} from '@/api/decode'
import type { Campaign, DeliveryStats } from '@/api/types'
import { APPROVAL_ID, CAMPAIGN_ID, deliveryStats, freshWorker, segmentedCampaign } from './helpers/fixtures'

function sendingCampaign(overrides: Partial<Campaign> = {}): Campaign {
  return segmentedCampaign({ status: 'sending', active_approval_id: APPROVAL_ID, version: 9, ...overrides })
}

function mixed(): DeliveryStats {
  return deliveryStats(sendingCampaign(), {
    channels: {
      sms: { pending: 3, in_flight: 1, sent: 5, failed: 1, skipped_consent_revoked: 1, uncertain: 1, attempts: { accepted: 5, timeout: 2, permanent_error: 1 } },
      email: { sent: 4, retry_scheduled: 2, cancelled: 0, skipped_no_contact: 1, attempts: { accepted: 3, replayed: 1, transient_error: 2 } },
    },
  })
}

function json<T>(v: T): T {
  return JSON.parse(JSON.stringify(v)) as T
}

describe('decodeDeliveryStats', () => {
  it('accepts consistent mixed-outcome statistics', () => {
    const st = decodeDeliveryStats(json(mixed()))
    expect(st.totals.approved_total).toBe(18)
    expect(st.channels.sms!.attempts.total).toBe(8)
    expect(st.processed + st.remaining).toBe(18)
    expect(st.outcome).toBe('sending')
  })

  it('accepts a campaign that never sent (empty snapshot, progress 0)', () => {
    const st = decodeDeliveryStats(json(deliveryStats(segmentedCampaign({ status: 'approved' }))))
    expect(st.approval_id).toBeNull()
    expect(st.channels).toEqual({})
    expect(st.progress).toBe(0)
  })

  const broken: Array<[string, (s: Record<string, any>) => void]> = [
    ['unknown top-level property', (s) => (s.recipients = [])],
    ['unknown channel property', (s) => (s.channels.sms.user_ids = [1])],
    ['unknown channel', (s) => (s.channels.push = s.channels.sms)],
    ['partition ≠ approved_total', (s) => (s.channels.sms.sent += 1)],
    ['totals ≠ Σ channels', (s) => { s.totals.sent += 1; s.totals.pending -= 1 }],
    ['uncertain above non-sent rows', (s) => (s.channels.email.uncertain = 99)],
    ['negative count', (s) => (s.channels.sms.pending = -1)],
    ['fractional count', (s) => (s.totals.pending = 1.5)],
    ['attempt total ≠ Σ by_outcome', (s) => (s.channels.sms.attempts.total += 1)],
    ['unknown attempt outcome', (s) => (s.channels.sms.attempts.by_outcome.delivered = 1)],
    ['sent without accepted attempts', (s) => { s.channels.sms.attempts.by_outcome.accepted = 0; s.channels.sms.attempts.by_outcome.unknown = 5 }],
    ['progress > 1', (s) => (s.progress = 1.5)],
    ['processed inconsistent', (s) => (s.processed = 1)],
    ['terminal while sending', (s) => (s.terminal = true)],
    ['wrong outcome', (s) => (s.outcome = 'completed')],
    ['campaign_terminal inconsistent', (s) => (s.campaign_terminal = true)],
    ['unsafe reason code', (s) => (s.channels.sms.reasons = [{ status: 'failed', code: '<b>x</b>', count: 1 }])],
    ['fresh worker without heartbeat', (s) => (s.worker = { ...s.worker, last_seen_at: null })],
    ['fresh worker beyond threshold', (s) => (s.worker = { ...s.worker, heartbeat_age_seconds: 99 })],
    ['channels without approval', (s) => (s.approval_id = null)],
    ['non-uuid campaign', (s) => (s.campaign_id = 'x')],
  ]
  it.each(broken)('rejects %s', (_name, mutate) => {
    const s = json(mixed()) as unknown as Record<string, any>
    mutate(s)
    expect(() => decodeDeliveryStats(s)).toThrow(DecodeError)
  })

  it('requires terminal exactly when a finished campaign has nothing remaining', () => {
    const settling = json(deliveryStats(sendingCampaign({ status: 'cancelled' }), { channels: { sms: { in_flight: 1, cancelled: 3 } } }))
    expect(decodeDeliveryStats(settling).outcome).toBe('settling')
    const done = json(deliveryStats(sendingCampaign({ status: 'completed_with_failures' }), { channels: { sms: { sent: 2, failed: 1 } } }))
    const st = decodeDeliveryStats(done)
    expect(st.terminal).toBe(true)
    expect(st.outcome).toBe('completed_with_failures')
    expect(() => decodeDeliveryStats({ ...done, terminal: false })).toThrow(DecodeError)
  })
})

describe('decodeDeliveryEvent', () => {
  it('decodes stats and end events and rejects anything else', () => {
    expect(decodeDeliveryEvent('stats', json(mixed())).type).toBe('stats')
    const end = { campaign_id: CAMPAIGN_ID, campaign_status: 'completed', campaign_version: 10, outcome: 'completed' }
    expect(decodeDeliveryEvent('end', end)).toEqual({ type: 'end', data: end })
    expect(() => decodeDeliveryEvent('end', { ...end, campaign_status: 'sending', outcome: 'sending' })).toThrow(DecodeError)
    expect(() => decodeDeliveryEvent('end', { ...end, outcome: 'cancelled' })).toThrow(DecodeError)
    expect(() => decodeDeliveryEvent('end', { ...end, extra: 1 })).toThrow(DecodeError)
    expect(() => decodeDeliveryEvent('delta', {})).toThrow(DecodeError)
    expect(() => decodeDeliveryEvent('stats', null)).toThrow(DecodeError)
  })
})

describe('decodeDeliveryPage', () => {
  const item = {
    id: 7, channel: 'sms', status: 'failed', attempts: 1, rate_limited_attempts: 0, outcome_uncertain: false,
    reason: 'destination_rejected', destination_masked: '+90********67', sent_at: null, updated_at: '2026-11-04T10:30:00Z',
  }
  const page = { campaign_id: CAMPAIGN_ID, approval_id: APPROVAL_ID, items: [item, { ...item, id: 9 }], next_cursor: '9', limit: 2 }

  it('accepts a page of safe rows', () => {
    expect(decodeDeliveryPage(json(page)).items.map((i) => i.id)).toEqual([7, 9])
    expect(decodeDeliveryPage({ ...page, approval_id: null, items: [], next_cursor: null }).items).toEqual([])
  })

  it.each([
    ['a raw destination', { ...item, destination_masked: '+905551234567' }],
    ['an unknown (sensitive) property', { ...item, user_id: 3 }],
    ['vars', { ...item, vars: { first_name: 'A' } }],
    ['an unsafe reason', { ...item, reason: 'Hello <script>' }],
    ['an unknown status', { ...item, status: 'held' }],
  ])('rejects %s', (_n, bad) => {
    expect(() => decodeDeliveryPage({ ...page, items: [bad], next_cursor: null })).toThrow(DecodeError)
  })

  it('rejects unordered ids, oversize pages and bad cursors', () => {
    expect(() => decodeDeliveryPage({ ...page, items: [{ ...item, id: 9 }, item] })).toThrow(DecodeError)
    expect(() => decodeDeliveryPage({ ...page, limit: 1 })).toThrow(DecodeError)
    expect(() => decodeDeliveryPage({ ...page, next_cursor: 'abc' })).toThrow(DecodeError)
  })
})

describe('send and cancel responses', () => {
  const c = sendingCampaign()
  const send = { status: 'sending', approval_id: APPROVAL_ID, released: { sms: 2, email: 3 }, already_started: false, campaign: c, server_time: '2026-11-04T10:25:00Z' }

  it('decodes started and already_started sends', () => {
    expect(decodeSendResponse(json(send)).released).toEqual({ sms: 2, email: 3 })
    expect(decodeSendResponse(json({ ...send, already_started: true, status: 'completed', campaign: { ...c, status: 'completed' } })).already_started).toBe(true)
  })

  it('rejects inconsistent send responses', () => {
    expect(() => decodeSendResponse({ ...send, status: 'approved', campaign: { ...c, status: 'approved' } })).toThrow(DecodeError)
    expect(() => decodeSendResponse({ ...send, status: 'completed' })).toThrow(DecodeError)
    expect(() => decodeSendResponse({ ...send, campaign: { ...c, active_approval_id: null } })).toThrow(DecodeError)
    expect(() => decodeSendResponse({ ...send, released: { push: 1 } })).toThrow(DecodeError)
    expect(() => decodeSendResponse({ ...send, recipients: [] })).toThrow(DecodeError)
  })

  it('decodes cancel responses', () => {
    const res = {
      status: 'cancelled', previous_status: 'sending', revoked_approval_id: null, released_held_rows: 0,
      cancelled_deliveries: { sms: 4 }, in_flight: { email: 1 }, campaign: { ...c, status: 'cancelled' }, server_time: '2026-11-04T10:25:00Z',
    }
    expect(decodeCancelResponse(json(res)).in_flight).toEqual({ email: 1 })
    expect(() => decodeCancelResponse({ ...res, campaign: c })).toThrow(DecodeError)
    expect(() => decodeCancelResponse({ ...res, in_flight: { sms: -1 } })).toThrow(DecodeError)
  })
})

describe('readiness', () => {
  it('reports the worker leniently: malformed is null, never fresh', () => {
    const base = { status: 'ready', database: 'ok', migration_version: 2, migration_dirty: false, llm_provider: 'demo' }
    expect(decodeReadiness({ ...base, worker: freshWorker(), sending_campaigns: 1 }).worker?.status).toBe('fresh')
    expect(decodeReadiness({ ...base, worker: { status: 'fresh' } }).worker).toBeNull()
    expect(decodeReadiness(base).worker).toBeNull()
    expect(decodeReadiness({ ...base, sending_campaigns: -1 }).sending_campaigns).toBeNull()
  })
})
