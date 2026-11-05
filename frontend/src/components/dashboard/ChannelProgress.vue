<script setup lang="ts">
// One channel's delivery partition as a stacked bar over approved_total,
// plus its counters. Uncertain is an overlay shown separately, never a
// segment of the bar.
import { computed } from 'vue'
import type { Channel, ChannelDeliveryStats } from '@/api/types'
import { CHANNEL_LABEL, formatCount, percent } from '@/composables/format'

const props = defineProps<{ channel: Channel; stats: ChannelDeliveryStats }>()

const segments = computed(() => {
  const s = props.stats
  const total = s.approved_total
  return [
    { key: 'sent', label: 'Sent', n: s.sent },
    { key: 'failed', label: 'Failed', n: s.failed },
    { key: 'skipped', label: 'Skipped', n: s.skipped_consent_revoked + s.skipped_no_contact },
    { key: 'cancelled', label: 'Cancelled', n: s.cancelled },
    { key: 'active', label: 'In progress', n: s.in_flight + s.retry_scheduled },
  ]
    .filter((x) => x.n > 0)
    .map((x) => ({ ...x, pct: percent(x.n, total) }))
})
const processed = computed(() => {
  const s = props.stats
  return s.sent + s.failed + s.skipped_consent_revoked + s.skipped_no_contact + s.cancelled
})
const pct = computed(() => Math.floor(percent(processed.value, props.stats.approved_total)))
</script>

<template>
  <div class="channel" :data-testid="`channel-progress-${channel}`">
    <div class="head">
      <h3 class="name"><span class="dot" :class="channel" aria-hidden="true" />{{ CHANNEL_LABEL[channel] }}</h3>
      <span class="summary num" :data-testid="`channel-processed-${channel}`">
        {{ formatCount(processed) }} / {{ formatCount(stats.approved_total) }} processed ({{ pct }}%)
      </span>
    </div>
    <div
      class="bar"
      role="progressbar"
      :aria-label="`${CHANNEL_LABEL[channel]} deliveries processed`"
      aria-valuemin="0"
      :aria-valuemax="stats.approved_total"
      :aria-valuenow="processed"
    >
      <span v-for="s in segments" :key="s.key" class="seg" :class="s.key" :style="{ width: `${s.pct}%` }" :title="`${s.label}: ${s.n}`" />
    </div>
    <dl class="counts">
      <div><dt>Sent</dt><dd class="num" :data-testid="`ch-${channel}-sent`">{{ formatCount(stats.sent) }}</dd></div>
      <div><dt>Failed</dt><dd class="num" :data-testid="`ch-${channel}-failed`">{{ formatCount(stats.failed) }}</dd></div>
      <div>
        <dt>Skipped</dt>
        <dd class="num" :data-testid="`ch-${channel}-skipped`">{{ formatCount(stats.skipped_consent_revoked + stats.skipped_no_contact) }}</dd>
      </div>
      <div><dt>Cancelled</dt><dd class="num" :data-testid="`ch-${channel}-cancelled`">{{ formatCount(stats.cancelled) }}</dd></div>
      <div>
        <dt>Pending</dt>
        <dd class="num" :data-testid="`ch-${channel}-pending`">{{ formatCount(stats.pending + stats.retry_scheduled + stats.in_flight) }}</dd>
      </div>
      <div><dt>Attempts</dt><dd class="num" :data-testid="`ch-${channel}-attempts`">{{ formatCount(stats.attempts.total) }}</dd></div>
    </dl>
  </div>
</template>

<style scoped>
.channel {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  min-width: 0;
}
.head {
  display: flex;
  flex-wrap: wrap;
  justify-content: space-between;
  gap: var(--space-2);
  align-items: baseline;
}
.name {
  font-size: 15px;
  font-weight: 650;
  display: flex;
  align-items: center;
  gap: 6px;
}
.dot.sms {
  background: var(--c-sms);
}
.dot.email {
  background: var(--c-email);
}
.summary {
  font-size: 13px;
  color: var(--c-muted);
}
.bar {
  display: flex;
  height: 12px;
  border-radius: 999px;
  overflow: hidden;
  background: var(--c-surface-2);
  border: 1px solid var(--c-border);
}
.seg {
  height: 100%;
  transition: width 0.3s ease;
}
.seg.sent {
  background: var(--c-eligible);
}
.seg.failed {
  background: var(--c-danger);
}
.seg.skipped {
  background: var(--c-no-consent);
}
.seg.cancelled {
  background: var(--c-no-contact);
}
.seg.active {
  background: var(--c-info-border);
}
.counts {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(84px, 1fr));
  gap: var(--space-2);
  margin: 0;
}
.counts div {
  min-width: 0;
}
.counts dt {
  font-size: 12px;
  color: var(--c-muted);
}
.counts dd {
  margin: 0;
  font-size: 16px;
  font-weight: 650;
}
</style>
