// Live delivery progress over the campaign status stream (GET /events,
// ARCHITECTURE §12): EventSource, every event a full snapshot read from
// committed database rows, so reconnecting needs no replay.
//
// Rules:
//   - Every event is decoded strictly (decodeDeliveryEvent). A malformed or
//     oversized event is never applied: the connection is dropped and
//     re-established (the next connection starts with a fresh snapshot).
//   - Events of a superseded connection (campaign switched, component
//     unmounted, reconnect) are ignored (connection generation), and a
//     snapshot is applied only for the campaign shown and only if it is not
//     older than the one shown (database time), so a late event can never
//     roll the view back.
//   - The browser's own EventSource retry is not relied upon: on any error
//     the source is closed and reopened with capped exponential backoff.
//   - "end" (terminal, sent once after the final snapshot) closes the
//     stream for good. A server close without "end" (lifetime limit,
//     restart) is a disconnect and is reconnected.
//   - refresh() reads GET /stats once (manual refresh, recovery).

import { onScopeDispose, ref, shallowRef, watch, type Ref } from 'vue'
import { ApiError, api, isApiError } from '@/api/client'
import { decodeDeliveryEvent } from '@/api/decode'
import type { DeliveryEndEvent, DeliveryStats } from '@/api/types'

/** The subset of EventSource used here (tests inject a fake). */
export interface EventSourceLike {
  readonly readyState: number
  onopen: ((ev: Event) => void) | null
  onerror: ((ev: Event) => void) | null
  addEventListener(type: string, listener: (ev: MessageEvent) => void): void
  close(): void
}

export type EventSourceFactory = (url: string) => EventSourceLike

/**
 * - idle: not connected (disabled or no campaign);
 * - connecting / reconnecting: a connection is being (re)established;
 * - live: the stream is open and delivering valid events;
 * - ended: the terminal event arrived; no further connection;
 * - polling: EventSource unavailable; GET /stats is polled instead.
 */
export type ConnectionState = 'idle' | 'connecting' | 'live' | 'reconnecting' | 'ended' | 'polling'

export interface CampaignEventsOptions {
  /** null = EventSource unavailable (polling fallback). Default: window.EventSource. */
  factory?: EventSourceFactory | null
  initialBackoffMs?: number
  maxBackoffMs?: number
  pollIntervalMs?: number
  /** Bound per event (the backend caps events at 256 KiB). */
  maxEventChars?: number
  onSnapshot?: (s: DeliveryStats) => void
  onEnd?: (e: DeliveryEndEvent) => void
}

function defaultFactory(): EventSourceFactory | null {
  if (typeof EventSource === 'undefined') return null
  return (url) => new EventSource(url)
}

function toApiError(e: unknown): ApiError {
  if (isApiError(e)) return e
  return new ApiError({ kind: 'network', code: 'network_error', message: 'Cannot reach the server.', retryable: true })
}

export function useCampaignEvents(campaignId: Ref<string | null>, enabled: Ref<boolean>, opts: CampaignEventsOptions = {}) {
  const factory = opts.factory === undefined ? defaultFactory() : opts.factory
  const initialBackoff = opts.initialBackoffMs ?? 1000
  const maxBackoff = opts.maxBackoffMs ?? 15_000
  const pollInterval = opts.pollIntervalMs ?? 3000
  const maxEventChars = opts.maxEventChars ?? 1 << 20

  const stats = shallowRef<DeliveryStats | null>(null)
  const end = shallowRef<DeliveryEndEvent | null>(null)
  const state = ref<ConnectionState>('idle')
  /** Consecutive failed connections (reset by a valid event). */
  const failures = ref(0)
  /** Events rejected by the decoder since the campaign was opened. */
  const rejectedEvents = ref(0)
  const lastEventAt = ref<number | null>(null)
  const refreshing = ref(false)
  const refreshError = shallowRef<ApiError | null>(null)

  let source: EventSourceLike | null = null
  let generation = 0
  let timer: ReturnType<typeof setTimeout> | null = null
  let refreshController: AbortController | null = null

  function clearTimer() {
    if (timer !== null) clearTimeout(timer)
    timer = null
  }

  function closeSource() {
    generation++
    const s = source
    source = null
    s?.close()
  }

  function stop() {
    closeSource()
    clearTimer()
  }

  /** Applies a snapshot for the campaign shown unless it is older than the current one. */
  function apply(s: DeliveryStats): boolean {
    if (s.campaign_id !== campaignId.value) return false
    const cur = stats.value
    if (cur && cur.campaign_id === s.campaign_id && Date.parse(s.updated_at) < Date.parse(cur.updated_at)) return false
    stats.value = s
    opts.onSnapshot?.(s)
    return true
  }

  function backoffDelay(): number {
    const n = Math.min(failures.value, 10)
    return Math.min(maxBackoff, initialBackoff * 2 ** n)
  }

  function scheduleReconnect() {
    clearTimer()
    if (state.value === 'ended' || !enabled.value || !campaignId.value) return
    const delay = backoffDelay()
    failures.value++
    state.value = 'reconnecting'
    timer = setTimeout(() => {
      timer = null
      connect()
    }, delay)
  }

  function finish(e: DeliveryEndEvent) {
    end.value = e
    state.value = 'ended'
    stop()
    opts.onEnd?.(e)
  }

  function onMessage(myGen: number, src: EventSourceLike, name: string, raw: unknown) {
    if (myGen !== generation || src !== source) return
    let ev
    try {
      if (typeof raw !== 'string' || raw.length > maxEventChars) throw new Error('oversized or non-text event')
      ev = decodeDeliveryEvent(name, JSON.parse(raw))
    } catch (e) {
      if (import.meta.env.DEV) console.warn('progress event did not match the contract:', (e as Error).message)
      rejectedEvents.value++
      closeSource()
      scheduleReconnect()
      return
    }
    const id = campaignId.value
    if (ev.data.campaign_id !== id) return
    failures.value = 0
    lastEventAt.value = Date.now()
    if (ev.type === 'stats') {
      state.value = 'live'
      apply(ev.data)
      return
    }
    finish(ev.data)
  }

  function connect() {
    clearTimer()
    const id = campaignId.value
    if (!id || !enabled.value || state.value === 'ended') return
    if (!factory) {
      state.value = 'polling'
      void poll(id)
      return
    }
    closeSource()
    const myGen = generation
    state.value = failures.value > 0 ? 'reconnecting' : 'connecting'
    let src: EventSourceLike
    try {
      src = factory(api.eventsUrl(id))
    } catch {
      scheduleReconnect()
      return
    }
    source = src
    src.onopen = () => {
      if (myGen !== generation || src !== source) return
      if (state.value !== 'ended') state.value = 'live'
    }
    src.onerror = () => {
      if (myGen !== generation || src !== source) return
      closeSource()
      scheduleReconnect()
    }
    src.addEventListener('stats', (ev) => onMessage(myGen, src, 'stats', ev.data))
    src.addEventListener('end', (ev) => onMessage(myGen, src, 'end', ev.data))
  }

  async function poll(id: string) {
    await refresh()
    if (id !== campaignId.value || !enabled.value || state.value !== 'polling') return
    if (stats.value?.terminal) {
      state.value = 'ended'
      return
    }
    timer = setTimeout(() => {
      timer = null
      void poll(id)
    }, pollInterval)
  }

  /** GET /stats once (manual refresh). Errors are shown, never retried in a loop. */
  async function refresh(): Promise<void> {
    const id = campaignId.value
    if (!id) return
    refreshController?.abort()
    const controller = new AbortController()
    refreshController = controller
    refreshing.value = true
    refreshError.value = null
    try {
      const s = await api.getStats(id, controller.signal)
      if (controller.signal.aborted || id !== campaignId.value) return
      apply(s)
    } catch (e) {
      const err = toApiError(e)
      if (err.isAbort || controller.signal.aborted || id !== campaignId.value) return
      refreshError.value = err
    } finally {
      if (refreshController === controller) {
        refreshController = null
        refreshing.value = false
      }
    }
  }

  /** Reconnects now (manual), resetting the backoff. */
  function reconnect() {
    if (state.value === 'ended') return
    failures.value = 0
    stop()
    connect()
  }

  watch(
    [campaignId, enabled],
    ([id], [prevId]) => {
      stop()
      if (id !== prevId) {
        refreshController?.abort()
        refreshController = null
        refreshing.value = false
        refreshError.value = null
        stats.value = null
        end.value = null
        failures.value = 0
        rejectedEvents.value = 0
        lastEventAt.value = null
        state.value = 'idle'
      }
      if (state.value === 'ended') return
      state.value = 'idle'
      if (enabled.value && id) connect()
    },
    { immediate: true },
  )

  onScopeDispose(() => {
    stop()
    refreshController?.abort()
    refreshController = null
  })

  return { stats, end, state, failures, rejectedEvents, lastEventAt, refreshing, refreshError, refresh, reconnect }
}
