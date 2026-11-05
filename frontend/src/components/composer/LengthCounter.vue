<script setup lang="ts">
// Worst-case length after personalization as computed by the backend
// (authoritative), next to the raw length of the typed text (UX only).
import { computed } from 'vue'
import type { LengthStats } from '@/api/types'

const props = defineProps<{
  stats: LengthStats | null
  /** Hard limit (from the server's limits). */
  limit: number
  typed: string
  checking: boolean
  testid: string
}>()

const typedChars = computed(() => [...props.typed.normalize('NFC').trim()].length)
const state = computed(() => {
  const s = props.stats
  if (!s) return 'unknown'
  if (s.chars_worst_case > s.limit) return 'over'
  if (s.chars_worst_case >= s.limit * 0.9) return 'near'
  return 'ok'
})
</script>

<template>
  <p class="counter" :data-state="state" :data-testid="testid" aria-live="polite">
    <span class="worst">
      Worst case after personalization:
      <strong v-if="stats" :data-testid="`${testid}-worst`">{{ stats.chars_worst_case }} / {{ stats.limit }}</strong>
      <strong v-else :data-testid="`${testid}-worst`">— / {{ limit }}</strong>
      <span v-if="checking" class="checking">checking…</span>
    </span>
    <span class="typed">{{ typedChars }} characters typed</span>
  </p>
</template>

<style scoped>
.counter {
  display: flex;
  flex-wrap: wrap;
  justify-content: space-between;
  gap: 2px var(--space-3);
  margin-top: 4px;
  font-size: 12.5px;
  color: var(--c-muted);
}
.counter[data-state='near'] .worst strong {
  color: var(--c-warning);
}
.counter[data-state='over'] .worst strong {
  color: var(--c-danger);
}
.worst strong {
  font-variant-numeric: tabular-nums;
  color: var(--c-text-2);
}
.checking {
  margin-left: 6px;
  font-style: italic;
}
</style>
