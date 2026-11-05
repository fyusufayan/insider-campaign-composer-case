<script setup lang="ts">
// Paged list of delivery rows that did not end sent (GET /deliveries).
// Shows delivery ids, channel, status, sanitized reason, attempt counts and
// the uncertainty flag — no recipient data (the masked destination the API
// returns is deliberately not displayed). Every load is bound to the
// campaign and filter it was made for (superseded loads are aborted and
// their results dropped).
import { onBeforeUnmount, ref, shallowRef, watch } from 'vue'
import { ApiError, api, isApiError } from '@/api/client'
import type { DeliveryItem, DeliveryStatus } from '@/api/types'
import { CHANNEL_LABEL, formatDateTime } from '@/composables/format'
import ErrorNotice from '@/components/ui/ErrorNotice.vue'
import { DELIVERY_STATUS_LABEL, reasonLabel } from './deliveryLabels'

const props = defineProps<{ campaignId: string; refreshKey: string }>()

const FILTERS: Array<{ value: DeliveryStatus | 'uncertain'; label: string }> = [
  { value: 'failed', label: 'Failed' },
  { value: 'skipped_consent_revoked', label: 'Skipped — consent' },
  { value: 'skipped_no_contact', label: 'Skipped — contact' },
  { value: 'cancelled', label: 'Cancelled' },
  { value: 'retry_scheduled', label: 'Retry scheduled' },
  { value: 'uncertain', label: 'Uncertain outcome' },
]
const PAGE = 25

const filter = ref<DeliveryStatus | 'uncertain'>('failed')
const items = shallowRef<DeliveryItem[]>([])
const nextCursor = ref<string | null>(null)
const loading = ref(false)
const error = shallowRef<ApiError | null>(null)
let controller: AbortController | null = null
let seq = 0

async function load(reset: boolean) {
  controller?.abort()
  const c = new AbortController()
  controller = c
  const mySeq = ++seq
  loading.value = true
  error.value = null
  const f = filter.value
  try {
    const page = await api.listDeliveries(
      props.campaignId,
      {
        ...(f === 'uncertain' ? { uncertain: true } : { status: f }),
        limit: PAGE,
        ...(reset || !nextCursor.value ? {} : { cursor: nextCursor.value }),
      },
      c.signal,
    )
    if (mySeq !== seq || page.campaign_id !== props.campaignId) return
    items.value = reset ? page.items : [...items.value, ...page.items]
    nextCursor.value = page.next_cursor
  } catch (e) {
    const err = isApiError(e) ? e : null
    if (mySeq !== seq || err?.isAbort) return
    error.value = err ?? new ApiError({ kind: 'network', code: 'network_error', message: 'Cannot reach the server.' })
  } finally {
    if (mySeq === seq) loading.value = false
  }
}

watch(
  () => [props.campaignId, filter.value, props.refreshKey] as const,
  () => {
    nextCursor.value = null
    void load(true)
  },
  { immediate: true },
)
onBeforeUnmount(() => {
  seq++
  controller?.abort()
})
</script>

<template>
  <section class="failures" data-testid="failure-table">
    <div class="head">
      <h3 class="title">Deliveries that were not sent</h3>
      <label class="filter">
        <span class="visually-hidden">Show</span>
        <select v-model="filter" class="input select" data-testid="failure-filter">
          <option v-for="f in FILTERS" :key="f.value" :value="f.value">{{ f.label }}</option>
        </select>
      </label>
    </div>
    <ErrorNotice v-if="error" :error="error" op="deliveries" tone="warning" @retry="load(true)" />
    <p v-else-if="!loading && items.length === 0" class="empty" data-testid="failure-empty">No deliveries in this category.</p>
    <div v-if="items.length" class="table-wrap">
      <table class="table">
        <thead>
          <tr>
            <th scope="col">Delivery</th>
            <th scope="col">Channel</th>
            <th scope="col">Status</th>
            <th scope="col">Reason</th>
            <th scope="col" class="numcol">Attempts</th>
            <th scope="col">Updated</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="it in items" :key="it.id" :data-testid="`failure-row-${it.id}`">
            <td class="mono">#{{ it.id }}</td>
            <td>{{ CHANNEL_LABEL[it.channel] }}</td>
            <td>
              {{ DELIVERY_STATUS_LABEL[it.status] }}
              <span v-if="it.outcome_uncertain" class="badge badge-warning" title="An attempt may have been delivered without confirmation">uncertain</span>
            </td>
            <td>{{ it.reason ? reasonLabel(it.reason) : '—' }}</td>
            <td class="numcol num">
              {{ it.attempts }}<span v-if="it.rate_limited_attempts" class="muted"> + {{ it.rate_limited_attempts }} × 429</span>
            </td>
            <td class="muted">{{ formatDateTime(it.updated_at) }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <div class="foot">
      <button
        v-if="nextCursor"
        type="button"
        class="btn btn-secondary btn-sm"
        :disabled="loading"
        data-testid="failure-more"
        @click="load(false)"
      >
        {{ loading ? 'Loading…' : 'Load more' }}
      </button>
      <span v-else-if="loading" class="muted" role="status"><span class="spinner" aria-hidden="true" /> Loading…</span>
    </div>
  </section>
</template>

<style scoped>
.failures {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  min-width: 0;
}
.head {
  display: flex;
  flex-wrap: wrap;
  justify-content: space-between;
  align-items: center;
  gap: var(--space-2);
}
.title {
  font-size: 15px;
  font-weight: 650;
}
.select {
  width: auto;
  max-width: 100%;
  padding: 6px 10px;
}
.table-wrap {
  overflow-x: auto;
  max-width: 100%;
  border: 1px solid var(--c-border);
  border-radius: var(--radius-sm);
}
.table {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
}
.table th,
.table td {
  text-align: left;
  padding: 8px 10px;
  border-bottom: 1px solid var(--c-border);
  white-space: nowrap;
}
.table td:nth-child(4) {
  white-space: normal;
  min-width: 160px;
}
.table th {
  font-weight: 600;
  color: var(--c-muted);
  background: var(--c-surface-2);
}
.numcol {
  text-align: right;
}
.muted,
.empty {
  color: var(--c-muted);
  font-size: 13px;
}
</style>
