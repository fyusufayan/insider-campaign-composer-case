// useCampaignEvents: EventSource lifecycle, strict decoding, stale-event
// guards, capped exponential reconnect, terminal close, dispose, polling
// fallback. Timers are fake; no real waits.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { effectScope, nextTick, ref } from 'vue'
import type { Campaign } from '@/api/types'
import { useCampaignEvents } from '@/composables/useCampaignEvents'
import { FakeBackend } from './helpers/fakeBackend'
import { FakeEventSource } from './helpers/fakeEventSource'
import { APPROVAL_ID, CAMPAIGN_ID, OTHER_ID, deliveryStats, segmentedCampaign } from './helpers/fixtures'

let fake: FakeBackend

function sending(id = CAMPAIGN_ID, overrides: Partial<Campaign> = {}): Campaign {
  return segmentedCampaign({ id, status: 'sending', active_approval_id: APPROVAL_ID, version: 9, ...overrides })
}
const at = (s: number) => new Date(Date.UTC(2026, 10, 4, 10, 30, s)).toISOString()
const snap = (c: Campaign, sent: number, updated = at(sent)) =>
  deliveryStats(c, { channels: { sms: { sent, pending: 10 - sent } }, updatedAt: updated })

function setup(opts: { id?: string; enabled?: boolean; factory?: null } = {}) {
  const id = ref<string | null>(opts.id ?? CAMPAIGN_ID)
  const enabled = ref(opts.enabled ?? true)
  const onEnd = vi.fn()
  const onSnapshot = vi.fn()
  const scope = effectScope()
  const ev = scope.run(() =>
    useCampaignEvents(id, enabled, {
      ...(opts.factory === null ? { factory: null } : { factory: (url: string) => new FakeEventSource(url) }),
      initialBackoffMs: 1000,
      maxBackoffMs: 8000,
      pollIntervalMs: 2000,
      onEnd,
      onSnapshot,
    }),
  )!
  return { id, enabled, ev, scope, onEnd, onSnapshot }
}

beforeEach(() => {
  fake = new FakeBackend()
  FakeEventSource.reset()
  vi.useFakeTimers()
})
afterEach(() => {
  vi.useRealTimers()
})

describe('useCampaignEvents', () => {
  it('connects to the campaign stream and applies valid snapshots', () => {
    const { ev, onSnapshot } = setup()
    const src = FakeEventSource.latest()
    expect(src.url).toBe(`/api/v1/campaigns/${CAMPAIGN_ID}/events`)
    expect(ev.state.value).toBe('connecting')
    src.connect()
    expect(ev.state.value).toBe('live')
    src.emit('stats', snap(sending(), 3))
    expect(ev.stats.value?.totals.sent).toBe(3)
    expect(onSnapshot).toHaveBeenCalledTimes(1)
  })

  it('never rolls back to an older snapshot and ignores other campaigns', () => {
    const { ev } = setup()
    const src = FakeEventSource.latest()
    src.emit('stats', snap(sending(), 5))
    src.emit('stats', snap(sending(), 2)) // older updated_at
    expect(ev.stats.value?.totals.sent).toBe(5)
    src.emit('stats', snap(sending(OTHER_ID), 9))
    expect(ev.stats.value?.campaign_id).toBe(CAMPAIGN_ID)
    expect(ev.stats.value?.totals.sent).toBe(5)
  })

  it('rejects a malformed event without applying it and reconnects for a fresh snapshot', () => {
    const { ev } = setup()
    const first = FakeEventSource.latest()
    first.emit('stats', snap(sending(), 4))
    const bad = snap(sending(), 6) as unknown as Record<string, any>
    bad.totals.sent = 99 // does not reconcile
    first.emit('stats', bad)
    expect(ev.stats.value?.totals.sent).toBe(4)
    expect(ev.rejectedEvents.value).toBe(1)
    expect(first.closed).toBe(true)
    first.emit('stats', snap(sending(), 7)) // a late event of the closed source
    expect(ev.stats.value?.totals.sent).toBe(4)
    first.emit('end', { campaign_id: CAMPAIGN_ID, campaign_status: 'completed', campaign_version: 10, outcome: 'completed' })
    expect(ev.state.value).toBe('reconnecting')
    vi.advanceTimersByTime(1000)
    expect(FakeEventSource.instances).toHaveLength(2)
    // A non-JSON and an unknown-named payload are rejected too.
    FakeEventSource.latest().emit('stats', '{not json')
    expect(ev.rejectedEvents.value).toBe(2)
  })

  it('reconnects with capped exponential backoff and resets it after a valid event', () => {
    const { ev } = setup()
    const delays: number[] = []
    for (let i = 0; i < 5; i++) {
      FakeEventSource.latest().fail()
      expect(ev.state.value).toBe('reconnecting')
      const before = FakeEventSource.instances.length
      let waited = 0
      while (FakeEventSource.instances.length === before) {
        vi.advanceTimersByTime(500)
        waited += 500
      }
      delays.push(waited)
    }
    expect(delays).toEqual([1000, 2000, 4000, 8000, 8000])
    FakeEventSource.latest().emit('stats', snap(sending(), 1))
    expect(ev.failures.value).toBe(0)
    FakeEventSource.latest().fail()
    vi.advanceTimersByTime(1000)
    expect(FakeEventSource.instances).toHaveLength(7)
  })

  it('closes for good on the end event', () => {
    const { ev, onEnd } = setup()
    const src = FakeEventSource.latest()
    const done = sending(CAMPAIGN_ID, { status: 'completed', version: 10 })
    src.emit('stats', deliveryStats(done, { channels: { sms: { sent: 10 } } }))
    src.emit('end', { campaign_id: CAMPAIGN_ID, campaign_status: 'completed', campaign_version: 10, outcome: 'completed' })
    expect(ev.state.value).toBe('ended')
    expect(ev.end.value?.campaign_status).toBe('completed')
    expect(src.closed).toBe(true)
    expect(onEnd).toHaveBeenCalledTimes(1)
    src.fail()
    vi.advanceTimersByTime(60_000)
    expect(FakeEventSource.instances).toHaveLength(1)
  })

  it('switches campaigns: closes the old stream and drops its events', async () => {
    const { id, ev } = setup()
    const a = FakeEventSource.latest()
    a.emit('stats', snap(sending(), 2))
    id.value = OTHER_ID
    await nextTick()
    expect(a.closed).toBe(true)
    expect(ev.stats.value).toBeNull()
    const b = FakeEventSource.latest()
    expect(b.url).toContain(OTHER_ID)
    a.emit('stats', snap(sending(), 3))
    expect(ev.stats.value).toBeNull()
    b.emit('stats', snap(sending(OTHER_ID), 1))
    expect(ev.stats.value?.campaign_id).toBe(OTHER_ID)
  })

  it('disconnects when disabled and on dispose, with no reconnect afterwards', async () => {
    const { enabled, scope } = setup()
    const a = FakeEventSource.latest()
    enabled.value = false
    await nextTick()
    expect(a.closed).toBe(true)
    enabled.value = true
    await nextTick()
    const b = FakeEventSource.latest()
    expect(b).not.toBe(a)
    b.fail() // schedules a reconnect …
    scope.stop() // … which dispose cancels
    vi.advanceTimersByTime(60_000)
    expect(FakeEventSource.instances).toHaveLength(2)
    expect(b.closed).toBe(true)
  })

  it('refresh() reads GET /stats once and applies it', async () => {
    const c = fake.seed(sending())
    fake.stats.set(c.id, snap(c, 6, at(30)))
    const { ev } = setup()
    await ev.refresh()
    expect(fake.callsTo('GET', /\/stats$/)).toHaveLength(1)
    expect(ev.stats.value?.totals.sent).toBe(6)
  })

  it('falls back to polling GET /stats without EventSource and stops at terminal', async () => {
    const c = fake.seed(sending())
    fake.stats.set(c.id, snap(c, 2, at(1)))
    const { ev } = setup({ factory: null })
    expect(ev.state.value).toBe('polling')
    await vi.advanceTimersByTimeAsync(0)
    expect(ev.stats.value?.totals.sent).toBe(2)
    fake.campaigns.set(c.id, { ...c, status: 'completed', version: 10 })
    fake.stats.set(c.id, deliveryStats({ ...c, status: 'completed', version: 10 }, { channels: { sms: { sent: 10 } }, updatedAt: at(40) }))
    await vi.advanceTimersByTimeAsync(2000)
    expect(ev.stats.value?.terminal).toBe(true)
    expect(ev.state.value).toBe('ended')
    const calls = fake.callsTo('GET', /\/stats$/).length
    await vi.advanceTimersByTimeAsync(10_000)
    expect(fake.callsTo('GET', /\/stats$/)).toHaveLength(calls)
  })
})
