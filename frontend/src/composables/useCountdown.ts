// Display-only countdown to a server timestamp. The client clock is aligned
// with the server clock by `offsetMs` (server − client, from the response's
// server_time). It never decides anything: the server compares expiry with
// its database clock; the UI only disables actions and asks the server to
// re-read when the deadline passes.

import { computed, onBeforeUnmount, ref, watch, type Ref } from 'vue'

export function useCountdown(target: Ref<string | null>, offsetMs: Ref<number>, onElapsed?: () => void, tickMs = 1000) {
  const now = ref(Date.now())
  const timer = setInterval(() => (now.value = Date.now()), tickMs)
  onBeforeUnmount(() => clearInterval(timer))

  const remainingMs = computed<number | null>(() => {
    if (!target.value) return null
    const t = Date.parse(target.value)
    if (Number.isNaN(t)) return null
    return t - (now.value + offsetMs.value)
  })
  const elapsed = computed(() => remainingMs.value !== null && remainingMs.value <= 0)

  // Fire once per target when it elapses.
  let firedFor: string | null = null
  watch(
    [elapsed, target],
    ([e, t]) => {
      if (e && t && firedFor !== t) {
        firedFor = t
        onElapsed?.()
      }
    },
    { immediate: true },
  )
  return { remainingMs, elapsed, now }
}

/** "14:59", "1:02:03", "23 h 59 min" style remaining time; "expired" at ≤ 0. */
export function formatRemaining(ms: number | null): string {
  if (ms === null) return '—'
  if (ms <= 0) return 'expired'
  const total = Math.floor(ms / 1000)
  const h = Math.floor(total / 3600)
  const m = Math.floor((total % 3600) / 60)
  const s = total % 60
  const pad = (n: number) => String(n).padStart(2, '0')
  return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`
}
