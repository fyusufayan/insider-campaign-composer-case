// The delivery step (CP17B) against the stateful fake backend and a fake
// EventSource: explicit Start sending with confirmation, idempotent and
// conflicting sends, lost responses, the live dashboard, cancellation
// (including racing completion), worker-offline feedback, final results,
// navigation/unmount guards and malformed stream events.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Campaign, DeliveryItem, DeliveryStats } from '@/api/types'
import { useCampaignStore } from '@/stores/campaign'
import { FakeBackend, deferred, json } from './helpers/fakeBackend'
import { FakeEventSource } from './helpers/fakeEventSource'
import {
  APPROVAL_ID,
  CAMPAIGN_ID,
  OTHER_ID,
  contentVersion,
  deliveryStats,
  errorEnvelope,
  freshWorker,
  segmentedCampaign,
  type ChannelInput,
} from './helpers/fixtures'
import { mountApp, settle, text, until, type MountedApp } from './helpers/mountApp'

let fake: FakeBackend
let app: MountedApp | null = null

beforeEach(() => {
  fake = new FakeBackend()
  FakeEventSource.reset()
  vi.stubGlobal('EventSource', FakeEventSource)
  localStorage.clear()
})
afterEach(() => {
  app?.wrapper.unmount()
  app = null
  vi.useRealTimers()
})

/** An approved campaign (confirmed approval, 24 h left): sms 251 + email 318. */
function approvedCampaign(id = CAMPAIGN_ID): Campaign {
  const c = fake.seed(segmentedCampaign({ id }))
  const { campaign: ready, version } = fake.commitVersion(c, contentVersion())
  const { campaign: pending, approval } = fake.seedPending(ready, version)
  const approved: Campaign = { ...pending, status: 'approved', version: pending.version + 1 }
  fake.campaigns.set(id, approved)
  fake.approvals.set(approval.approval_id, {
    ...approval,
    status: 'approved',
    campaign_version: approved.version,
    approved_at: new Date().toISOString(),
    approved_by: 'marketer@demo',
    expires_at: new Date(Date.now() + 24 * 3_600_000).toISOString(),
  })
  return approved
}

/** A campaign already sending (or finished) with its statistics. */
function sendingCampaign(
  opts: { id?: string; status?: Campaign['status']; channels?: Partial<Record<'sms' | 'email', ChannelInput>>; worker?: DeliveryStats['worker'] } = {},
): Campaign {
  const id = opts.id ?? CAMPAIGN_ID
  const c = fake.seed(segmentedCampaign({ id, status: opts.status ?? 'sending', active_approval_id: APPROVAL_ID, version: 9, current_content_id: 101 }))
  fake.stats.set(id, deliveryStats(c, { channels: opts.channels ?? { sms: { pending: 6, sent: 4 } }, ...(opts.worker ? { worker: opts.worker } : {}) }))
  return c
}

async function open(id = CAMPAIGN_ID): Promise<MountedApp> {
  app = await mountApp(`/campaigns/${id}`)
  await until(() => expect(find('delivery-panel').exists()).toBe(true))
  return app
}

function find(testid: string) {
  return app!.wrapper.find(`[data-testid="${testid}"]`)
}

/** The open stream of a campaign (the test plays the server). */
function streamOf(id = CAMPAIGN_ID): FakeEventSource {
  const s = FakeEventSource.open().filter((x) => x.url.includes(id))
  expect(s.length, `open streams for ${id}`).toBe(1)
  return s[0]!
}

function push(id: string, st: DeliveryStats) {
  fake.stats.set(id, st)
  streamOf(id).emit('stats', st)
}

function finish(id: string, status: Campaign['status'], channels: Partial<Record<'sms' | 'email', ChannelInput>>) {
  const c = fake.campaigns.get(id)!
  const done: Campaign = { ...c, status, version: c.version + 1 }
  fake.campaigns.set(id, done)
  const st = deliveryStats(done, { channels })
  push(id, st)
  streamOf(id).emit('end', { campaign_id: id, campaign_status: status, campaign_version: done.version, outcome: status })
}

describe('start sending', () => {
  it('never sends implicitly: opening, refreshing or approving does not call /send', async () => {
    approvedCampaign()
    await open()
    await until(() => expect(find('start-sending').attributes('disabled')).toBeUndefined())
    await useCampaignStore().reload()
    await settle()
    expect(fake.callsTo('POST', /\/send$/)).toHaveLength(0)
    expect(FakeEventSource.instances).toHaveLength(0) // no stream before sending
  })

  it('asks for explicit confirmation, sends once for repeated clicks and switches to the live dashboard', async () => {
    const c = approvedCampaign()
    await open()
    await until(() => expect(find('start-sending').attributes('disabled')).toBeUndefined())
    expect(text(app!.wrapper, 'send-snapshot-count')).toBe('569')
    await find('start-sending').trigger('click')
    expect(find('send-dialog').exists()).toBe(true)
    expect(text(app!.wrapper, 'send-dialog')).toMatch(/cannot be undone/)
    expect(text(app!.wrapper, 'send-dialog')).toMatch(/may start sending before this confirmation finishes/)
    expect(text(app!.wrapper, 'send-dialog')).toMatch(/SMS 251, Email 318/)
    expect(fake.callsTo('POST', /\/send$/)).toHaveLength(0)

    const gate = deferred<void>()
    fake.override('POST', /\/send$/, async () => {
      await gate.promise
      return undefined // fall through to the stateful route
    })
    await find('send-dialog-confirm').trigger('click')
    await find('send-dialog-confirm').trigger('click')
    await useCampaignStore().startSending()
    expect(find('send-dialog-confirm').attributes('disabled')).toBeDefined()
    gate.resolve()
    await until(() => expect(useCampaignStore().campaign?.status).toBe('sending'))
    const posts = fake.callsTo('POST', /\/send$/)
    expect(posts).toHaveLength(1)
    expect(posts[0]!.body).toEqual({ approval_id: APPROVAL_ID, expected_version: c.version })
    await until(() => expect(find('delivery-notice').attributes('data-kind')).toBe('started'))
    expect(text(app!.wrapper, 'delivery-notice')).toMatch(/569 deliveries were released/)
    expect(find('send-dialog').exists()).toBe(false)

    // Live dashboard: stream opened, snapshots advance the counters.
    await until(() => expect(FakeEventSource.open()).toHaveLength(1))
    const sending = fake.campaigns.get(CAMPAIGN_ID)!
    push(CAMPAIGN_ID, deliveryStats(sending, { channels: { sms: { pending: 251 }, email: { pending: 318 } } }))
    await until(() => expect(text(app!.wrapper, 'overall-progress')).toBe('0 / 569 (0%)'))
    push(CAMPAIGN_ID, deliveryStats(sending, { channels: { sms: { pending: 151, in_flight: 10, sent: 90 }, email: { pending: 318 } } }))
    await until(() => expect(text(app!.wrapper, 'tile-sent')).toBe('90'))
    expect(text(app!.wrapper, 'overall-progress')).toBe('90 / 569 (15%)')
    expect(text(app!.wrapper, 'ch-sms-pending')).toBe('161')

    // Completion with failures: final summary, stream closed, campaign re-read.
    const gets = fake.callsTo('GET', new RegExp(`/campaigns/${CAMPAIGN_ID}$`)).length
    finish(CAMPAIGN_ID, 'completed_with_failures', { sms: { sent: 249, failed: 2 }, email: { sent: 310, skipped_consent_revoked: 5, skipped_no_contact: 3 } })
    await until(() => expect(find('final-summary').attributes('data-outcome')).toBe('completed_with_failures'))
    expect(text(app!.wrapper, 'final-summary')).toMatch(/559 sent, 2 failed, 8 skipped, 0 cancelled of 569/)
    expect(FakeEventSource.open()).toHaveLength(0)
    await until(() => expect(useCampaignStore().campaign?.status).toBe('completed_with_failures'))
    expect(fake.callsTo('GET', new RegExp(`/campaigns/${CAMPAIGN_ID}$`)).length).toBeGreaterThan(gets)
    expect(find('cancel-sending').exists()).toBe(false)
  })

  it('treats already_started as success', async () => {
    approvedCampaign()
    await open()
    await until(() => expect(find('start-sending').attributes('disabled')).toBeUndefined())
    fake.override('POST', /\/send$/, () => {
      const c = fake.campaigns.get(CAMPAIGN_ID)!
      const next = { ...c, status: 'sending' as const, version: c.version + 1 }
      fake.campaigns.set(CAMPAIGN_ID, next)
      return json(200, { status: 'sending', approval_id: APPROVAL_ID, released: { sms: 251, email: 318 }, already_started: true, campaign: next, server_time: new Date().toISOString() })
    })
    await useCampaignStore().startSending()
    await until(() => expect(find('delivery-notice').attributes('data-kind')).toBe('already_started'))
    expect(useCampaignStore().campaign?.status).toBe('sending')
    expect(find('delivery-action-error').exists()).toBe(false)
  })

  it('shows a stale-version 409 with a reload path and changes nothing', async () => {
    approvedCampaign()
    await open()
    await until(() => expect(find('start-sending').attributes('disabled')).toBeUndefined())
    fake.override('POST', /\/send$/, () => json(409, errorEnvelope('version_conflict', 'the campaign was changed by another request; reload it and try again')))
    await useCampaignStore().startSending()
    await settle()
    expect(find('delivery-action-error').attributes('data-code')).toBe('version_conflict')
    expect(text(app!.wrapper, 'delivery-action-error')).toMatch(/Nothing was started/)
    expect(useCampaignStore().campaign?.status).toBe('approved')
    expect(fake.callsTo('POST', /\/send$/)).toHaveLength(1) // never retried
  })

  it('reconciles a lost send response with a fresh GET instead of guessing', async () => {
    approvedCampaign()
    await open()
    await until(() => expect(find('start-sending').attributes('disabled')).toBeUndefined())
    fake.override('POST', /\/send$/, () => {
      // The server started the send, but the response never arrives.
      const c = fake.campaigns.get(CAMPAIGN_ID)!
      fake.campaigns.set(CAMPAIGN_ID, { ...c, status: 'sending', version: c.version + 1 })
      throw new TypeError('Failed to fetch')
    })
    await useCampaignStore().startSending()
    await until(() => expect(useCampaignStore().campaign?.status).toBe('sending'))
    expect(find('delivery-action-error').attributes('data-code')).toBe('network_error')
    expect(text(app!.wrapper, 'delivery-action-error')).toMatch(/may or may not have started/)
    await until(() => expect(find('delivery-notice').attributes('data-kind')).toBe('reconciled'))
    expect(fake.callsTo('POST', /\/send$/)).toHaveLength(1)
  })
})

describe('dashboard on load', () => {
  it('restores a sending campaign from the stream, with manual refresh', async () => {
    sendingCampaign({ channels: { sms: { pending: 6, sent: 4 }, email: { pending: 2, sent: 1 } } })
    await open()
    await until(() => expect(FakeEventSource.open()).toHaveLength(1))
    expect(find('stats-loading').exists()).toBe(true)
    streamOf().connect()
    streamOf().emit('stats', fake.stats.get(CAMPAIGN_ID))
    await until(() => expect(text(app!.wrapper, 'overall-progress')).toBe('5 / 13 (38%)'))
    expect(find('connection-badge').attributes('data-state')).toBe('live')
    expect(find('cancel-sending').exists()).toBe(true)
    // Manual refresh reads GET /stats.
    const c = fake.campaigns.get(CAMPAIGN_ID)!
    fake.stats.set(CAMPAIGN_ID, deliveryStats(c, { channels: { sms: { pending: 1, sent: 9 }, email: { sent: 3 } } }))
    await find('stats-refresh').trigger('click')
    await until(() => expect(text(app!.wrapper, 'tile-sent')).toBe('12'))
    expect(fake.callsTo('GET', /\/stats$/)).toHaveLength(1)
    expect(fake.callsTo('POST', /\/send$/)).toHaveLength(0)
  })

  it('shows final results of a completed campaign read-only, and a campaign cancelled before sending', async () => {
    sendingCampaign({ status: 'completed', channels: { sms: { sent: 10 } } })
    await open()
    await until(() => expect(FakeEventSource.open()).toHaveLength(1))
    streamOf().emit('stats', fake.stats.get(CAMPAIGN_ID))
    streamOf().emit('end', { campaign_id: CAMPAIGN_ID, campaign_status: 'completed', campaign_version: 9, outcome: 'completed' })
    await until(() => expect(find('final-summary').attributes('data-outcome')).toBe('completed'))
    expect(find('connection-badge').attributes('data-state')).toBe('ended')
    expect(find('cancel-sending').exists()).toBe(false)
    expect(find('start-sending').exists()).toBe(false)
    app!.wrapper.unmount()
    app = null

    const c = fake.seed(segmentedCampaign({ id: OTHER_ID, status: 'cancelled', version: 4 }))
    fake.stats.set(OTHER_ID, deliveryStats(c))
    await open(OTHER_ID)
    await until(() => expect(FakeEventSource.open().length).toBe(1))
    streamOf(OTHER_ID).emit('stats', fake.stats.get(OTHER_ID))
    await until(() => expect(find('cancelled-before-send').exists()).toBe(true))
  })

  it('warns when the worker heartbeat is stale while sending', async () => {
    sendingCampaign({ worker: freshWorker({ status: 'stale', heartbeat_age_seconds: 42 }) })
    await open()
    await until(() => expect(FakeEventSource.open()).toHaveLength(1))
    streamOf().emit('stats', fake.stats.get(CAMPAIGN_ID))
    await until(() => expect(find('worker-offline').exists()).toBe(true))
    expect(text(app!.wrapper, 'worker-offline')).toMatch(/last seen 42 s ago/)
    expect(text(app!.wrapper, 'worker-offline')).toMatch(/nothing is lost/)
    expect(find('worker-status').attributes('data-status')).toBe('stale')
    const c = fake.campaigns.get(CAMPAIGN_ID)!
    push(CAMPAIGN_ID, deliveryStats(c, { channels: { sms: { pending: 5, sent: 5 } }, worker: freshWorker() }))
    await until(() => expect(find('worker-offline').exists()).toBe(false))
  })

  it('lists failed and uncertain deliveries without recipient data and flags uncertainty', async () => {
    sendingCampaign({
      status: 'completed_with_failures',
      channels: { sms: { sent: 7, failed: 2, uncertain: 1, reasons: [{ status: 'failed', code: 'destination_rejected', count: 2 }] } },
    })
    const rows: DeliveryItem[] = [3, 8].map((id) => ({
      id, channel: 'sms', status: 'failed', attempts: 5, rate_limited_attempts: id === 8 ? 2 : 0, outcome_uncertain: id === 8,
      reason: 'destination_rejected', destination_masked: '+90********67', sent_at: null, updated_at: '2026-11-04T10:40:00Z',
    }))
    fake.deliveries.set(CAMPAIGN_ID, rows)
    await open()
    await until(() => expect(FakeEventSource.open()).toHaveLength(1))
    streamOf().emit('stats', fake.stats.get(CAMPAIGN_ID))
    await until(() => expect(find('failure-row-8').exists()).toBe(true))
    expect(text(app!.wrapper, 'failure-row-8')).toMatch(/Provider rejected the destination/)
    expect(text(app!.wrapper, 'failure-row-8')).toMatch(/uncertain/)
    expect(text(app!.wrapper, 'failure-row-8')).toMatch(/5 \+ 2 × 429/)
    expect(text(app!.wrapper, 'uncertain-note')).toMatch(/1 uncertain outcome/)
    expect(text(app!.wrapper, 'reason-summary')).toMatch(/2 SMS · Failed — Provider rejected the destination/)
    // No recipient-level data, not even masked destinations.
    expect(app!.wrapper.text()).not.toContain('+90')
    expect(fake.callsTo('GET', /\/deliveries$/)[0]).toBeDefined()
    expect(fake.lastUrl).toContain('status=failed')
    // Responsive containers: the table scrolls inside its own wrapper.
    expect(find('failure-table').find('.table-wrap table').exists()).toBe(true)
  })
})

describe('cancel', () => {
  it('explains the in-flight limit, posts with the version shown and reports the counts', async () => {
    const c = sendingCampaign({ channels: { sms: { pending: 6, in_flight: 2, sent: 4 } } })
    await open()
    await until(() => expect(FakeEventSource.open()).toHaveLength(1))
    streamOf().emit('stats', fake.stats.get(CAMPAIGN_ID))
    await until(() => expect(find('cancel-sending').exists()).toBe(true))
    await find('cancel-sending').trigger('click')
    expect(text(app!.wrapper, 'cancel-dialog')).toMatch(/cannot be recalled/)
    expect(text(app!.wrapper, 'cancel-dialog')).toMatch(/already sent stay sent/)
    await find('cancel-dialog-confirm').trigger('click')
    await until(() => expect(find('delivery-notice').attributes('data-kind')).toBe('cancelled'))
    expect(fake.callsTo('POST', /\/cancel$/)[0]!.body).toEqual({ expected_version: c.version })
    expect(text(app!.wrapper, 'delivery-notice')).toMatch(/6 queued deliveries were cancelled/)
    expect(text(app!.wrapper, 'delivery-notice')).toMatch(/Up to 2 requests already in flight/)
    expect(useCampaignStore().campaign?.status).toBe('cancelled')
    // The stream shows the cancelled campaign settling, then the final numbers.
    push(CAMPAIGN_ID, fake.stats.get(CAMPAIGN_ID)!)
    await until(() => expect(find('settling').exists()).toBe(true))
    finish(CAMPAIGN_ID, 'cancelled', { sms: { sent: 6, cancelled: 6 } })
    await until(() => expect(find('final-summary').attributes('data-outcome')).toBe('cancelled'))
    expect(text(app!.wrapper, 'final-summary')).toMatch(/6 sent, 0 failed, 0 skipped, 6 cancelled of 12/)
  })

  it('reconciles a cancel that raced completion (409 invalid_state) and a lost cancel response', async () => {
    sendingCampaign()
    await open()
    await until(() => expect(FakeEventSource.open()).toHaveLength(1))
    streamOf().emit('stats', fake.stats.get(CAMPAIGN_ID))
    await until(() => expect(find('cancel-sending').exists()).toBe(true))
    // The worker completed the campaign just before the cancel arrived.
    const c = fake.campaigns.get(CAMPAIGN_ID)!
    fake.campaigns.set(CAMPAIGN_ID, { ...c, status: 'completed', version: c.version + 1 })
    await useCampaignStore().cancelSending()
    await until(() => expect(useCampaignStore().campaign?.status).toBe('completed'))
    expect(find('delivery-action-error').attributes('data-code')).toBe('invalid_state')
    expect(find('cancel-sending').exists()).toBe(false)
    app!.wrapper.unmount()
    app = null

    const other = sendingCampaign({ id: OTHER_ID })
    await open(OTHER_ID)
    await until(() => expect(useCampaignStore().canCancelSending).toBe(true))
    fake.override('POST', /\/cancel$/, () => {
      fake.campaigns.set(OTHER_ID, { ...other, status: 'cancelled', version: other.version + 1 })
      throw new TypeError('Failed to fetch')
    })
    await useCampaignStore().cancelSending()
    await until(() => expect(useCampaignStore().campaign?.status).toBe('cancelled'))
    expect(find('delivery-action-error').attributes('data-code')).toBe('network_error')
    expect(fake.callsTo('POST', new RegExp(`${OTHER_ID}/cancel$`))).toHaveLength(1) // never retried
  })
})

describe('stream safety', () => {
  it('drops events of a previous campaign after navigation and closes streams on unmount', async () => {
    sendingCampaign({ channels: { sms: { pending: 5, sent: 5 } } })
    sendingCampaign({ id: OTHER_ID, channels: { email: { pending: 1, sent: 1 } } })
    await open()
    await until(() => expect(FakeEventSource.open()).toHaveLength(1))
    const a = streamOf(CAMPAIGN_ID)
    a.emit('stats', fake.stats.get(CAMPAIGN_ID))
    await until(() => expect(text(app!.wrapper, 'tile-sent')).toBe('5'))
    await app!.router.push(`/campaigns/${OTHER_ID}`)
    await until(() => expect(useCampaignStore().campaign?.id).toBe(OTHER_ID))
    expect(a.closed).toBe(true)
    a.emit('stats', deliveryStats(fake.campaigns.get(CAMPAIGN_ID)!, { channels: { sms: { sent: 10 } } }))
    await until(() => expect(FakeEventSource.open()).toHaveLength(1))
    streamOf(OTHER_ID).emit('stats', fake.stats.get(OTHER_ID))
    await until(() => expect(text(app!.wrapper, 'tile-sent')).toBe('1'))
    app!.wrapper.unmount()
    app = null
    expect(FakeEventSource.open()).toHaveLength(0)
  })

  it('never applies a malformed event or marks the campaign complete from it', async () => {
    sendingCampaign()
    await open()
    await until(() => expect(FakeEventSource.open()).toHaveLength(1))
    const s = streamOf()
    s.emit('stats', fake.stats.get(CAMPAIGN_ID))
    await until(() => expect(text(app!.wrapper, 'tile-sent')).toBe('4'))
    const forged = deliveryStats({ ...fake.campaigns.get(CAMPAIGN_ID)!, status: 'completed' }, { channels: { sms: { sent: 10 } } }) as unknown as Record<string, any>
    forged.terminal = false // inconsistent
    s.emit('stats', forged)
    s.emit('end', { campaign_id: CAMPAIGN_ID, campaign_status: 'completed' }) // incomplete
    await settle()
    expect(text(app!.wrapper, 'tile-sent')).toBe('4')
    expect(find('final-summary').exists()).toBe(false)
    expect(useCampaignStore().campaign?.status).toBe('sending')
    expect(find('stream-rejected').exists()).toBe(true)
    expect(find('connection-badge').attributes('data-state')).toBe('reconnecting')
  })
})
