<script setup lang="ts">
// Step 7: send and delivery progress (CP17B). An approved campaign offers
// Start sending behind an explicit confirmation; a sending or finished
// campaign shows the live dashboard: numbers come only from committed
// database rows (GET /stats snapshots over the progress stream), so a
// delivery is shown as sent only after the worker recorded it. No
// recipient data and no message text is shown here.
import { computed, nextTick, ref, watch } from 'vue'
import { storeToRefs } from 'pinia'
import { useCampaignStore } from '@/stores/campaign'
import { useCampaignEvents, type EventSourceFactory } from '@/composables/useCampaignEvents'
import { CHANNELS, type Channel } from '@/api/types'
import { CHANNEL_LABEL, formatCount, formatDateTime } from '@/composables/format'
import ConfirmDialog from '@/components/ui/ConfirmDialog.vue'
import ErrorNotice from '@/components/ui/ErrorNotice.vue'
import ChannelProgress from './ChannelProgress.vue'
import FailureTable from './FailureTable.vue'
import { CONNECTION_LABEL, DELIVERY_STATUS_LABEL, WORKER_LABEL, reasonLabel } from './deliveryLabels'

const props = defineProps<{
  /** Injected in tests; default: window.EventSource (null = polling fallback). */
  eventSourceFactory?: EventSourceFactory | null
}>()

const store = useCampaignStore()
const {
  campaign,
  liveApproval,
  sending,
  cancelling,
  busy,
  canSend,
  sendBlockedReason,
  canCancelSending,
  deliveryActionError,
  deliveryNotice,
} = storeToRefs(store)

const status = computed(() => campaign.value?.status ?? null)
const streaming = computed(
  () => !!status.value && ['sending', 'completed', 'completed_with_failures', 'cancelled'].includes(status.value),
)
const campaignId = computed(() => campaign.value?.id ?? null)
const events = useCampaignEvents(campaignId, streaming, {
  ...(props.eventSourceFactory !== undefined ? { factory: props.eventSourceFactory } : {}),
  onSnapshot: (s) => store.noteServerVersion(s.campaign_version),
  onEnd: (e) => store.noteServerVersion(e.campaign_version),
})
const { stats, state: connection, refreshing, refreshError, rejectedEvents } = events

/** Only a snapshot of the campaign shown is displayed. */
const view = computed(() => (stats.value && stats.value.campaign_id === campaignId.value ? stats.value : null))
const channelsShown = computed<Channel[]>(() => CHANNELS.filter((c) => !!view.value?.channels[c]))
const progressPct = computed(() => Math.floor((view.value?.progress ?? 0) * 100))
const skipped = computed(() => (view.value ? view.value.totals.skipped_consent_revoked + view.value.totals.skipped_no_contact : 0))
const rateLimited = computed(() => view.value?.attempts.by_outcome.rate_limited ?? 0)
const reasons = computed(() => {
  const v = view.value
  if (!v) return []
  const out: Array<{ channel: Channel; status: string; code: string; count: number }> = []
  for (const ch of CHANNELS) for (const r of v.channels[ch]?.reasons ?? []) out.push({ channel: ch, status: r.status, code: r.code, count: r.count })
  return out
})
const workerProblem = computed(() => {
  const v = view.value
  return !!v && v.campaign_status === 'sending' && v.worker.status !== 'fresh'
})
const failureKey = computed(() => {
  const t = view.value?.totals
  if (!t) return ''
  return [t.failed, t.skipped_consent_revoked, t.skipped_no_contact, t.cancelled, t.retry_scheduled, t.uncertain, view.value?.terminal].join(':')
})

// Approved: what Start sending would release (the frozen snapshot).
const snapshot = computed(() => {
  const a = liveApproval.value
  if (!a || a.status !== 'approved') return null
  return {
    total: a.snapshot_count,
    perChannel: CHANNELS.filter((c) => a.channels.includes(c)).map((c) => ({ channel: c, n: a.snapshot[c]?.recipients ?? 0 })),
    contentVersion: a.content_version,
    approvalId: a.approval_id,
  }
})

const confirmSend = ref(false)
const confirmCancel = ref(false)
const panel = ref<HTMLElement | null>(null)

async function doSend() {
  await store.startSending()
  confirmSend.value = false
}
async function doCancel() {
  await store.cancelSending()
  confirmCancel.value = false
}

// Transition to the dashboard once sending has started.
watch(status, async (now, before) => {
  if (before === 'approved' && now === 'sending') {
    await nextTick()
    panel.value?.scrollIntoView?.({ behavior: 'smooth', block: 'start' })
  }
  if (now !== 'approved') confirmSend.value = false
  if (now !== 'sending') confirmCancel.value = false
})

const finalTone = computed(() => {
  const o = view.value?.outcome
  if (o === 'completed') return 'notice-success'
  if (o === 'completed_with_failures') return 'notice-warning'
  return 'notice-info'
})
</script>

<template>
  <section ref="panel" class="card delivery-panel" data-testid="delivery-panel">
    <div class="card-header header">
      <p class="card-eyebrow"><span class="step-num">7</span> Send &amp; delivery</p>
      <div v-if="streaming" class="conn">
        <span class="badge" :class="connection === 'live' || connection === 'ended' ? 'badge-success' : 'badge-warning'" data-testid="connection-badge" :data-state="connection">
          {{ CONNECTION_LABEL[connection] }}
        </span>
        <button type="button" class="btn btn-ghost btn-sm" :disabled="refreshing" data-testid="stats-refresh" @click="events.refresh()">
          {{ refreshing ? 'Refreshing…' : 'Refresh' }}
        </button>
        <button v-if="connection === 'reconnecting'" type="button" class="btn btn-ghost btn-sm" data-testid="stream-reconnect" @click="events.reconnect()">
          Reconnect now
        </button>
      </div>
    </div>

    <div class="card-body body">
      <div v-if="deliveryNotice" class="notice notice-info" role="status" data-testid="delivery-notice" :data-kind="deliveryNotice.kind">
        <div>
          <template v-if="deliveryNotice.kind === 'started'">
            <p class="notice-title">Sending started</p>
            <p>{{ formatCount(deliveryNotice.released) }} deliveries were released to the queue. Progress below follows the worker's recorded results.</p>
          </template>
          <template v-else-if="deliveryNotice.kind === 'already_started'">
            <p class="notice-title">Sending had already started</p>
            <p>This approval was already used to start sending; nothing new was started.</p>
          </template>
          <template v-else-if="deliveryNotice.kind === 'cancelled'">
            <p class="notice-title">Campaign cancelled</p>
            <p>
              {{ formatCount(deliveryNotice.cancelled) }} queued deliveries were cancelled.
              <template v-if="deliveryNotice.inFlight > 0">
                Up to {{ formatCount(deliveryNotice.inFlight) }} requests already in flight may still reach the provider;
                they cannot be recalled and their real outcome is recorded.
              </template>
              <template v-else>No request was in flight.</template>
              Deliveries already sent stay sent.
            </p>
          </template>
          <template v-else>
            <p class="notice-title">Campaign state reloaded</p>
            <p>The {{ deliveryNotice.op === 'send' ? 'send' : 'cancel' }} response was not received, so the latest state was read from the server.</p>
          </template>
        </div>
        <button type="button" class="btn btn-ghost btn-sm" @click="store.dismissDeliveryNotice()">Dismiss</button>
      </div>

      <ErrorNotice
        v-if="deliveryActionError"
        :error="deliveryActionError.error"
        :op="deliveryActionError.op"
        :busy="busy"
        retry-label="Dismiss"
        data-testid="delivery-action-error"
        @retry="store.dismissDeliveryError()"
        @reload="store.reload()"
      />

      <!-- Approved: start sending -->
      <template v-if="status === 'approved'">
        <p class="text">
          Starting the send releases the frozen snapshot<template v-if="snapshot"> of
          <strong data-testid="send-snapshot-count">{{ formatCount(snapshot.total) }}</strong> deliveries</template>
          to the delivery queue. The worker sends through the <strong>mock provider</strong> (no real SMS or email),
          at most 100 requests per second per channel, re-checking consent and contact before every attempt.
        </p>
        <div class="actions">
          <button type="button" class="btn btn-primary" :disabled="!canSend" data-testid="start-sending" @click="confirmSend = true">
            Start sending…
          </button>
          <span v-if="sendBlockedReason && !sending" class="hint" data-testid="send-blocked">{{ sendBlockedReason }}</span>
        </div>
      </template>

      <p v-else-if="!streaming" class="hint" data-testid="delivery-not-ready">Confirm an approval to send this campaign.</p>

      <!-- Sending / finished: dashboard -->
      <template v-else>
        <p v-if="!view && !refreshError" class="hint" role="status" data-testid="stats-loading">
          <span class="spinner" aria-hidden="true" /> Loading delivery statistics…
        </p>
        <ErrorNotice v-if="refreshError" :error="refreshError" op="stats" tone="warning" @retry="events.refresh()" />
        <p v-if="rejectedEvents > 0" class="hint" data-testid="stream-rejected">
          {{ rejectedEvents }} invalid progress update{{ rejectedEvents === 1 ? ' was' : 's were' }} ignored; the view reconnected for a fresh snapshot.
        </p>

        <template v-if="view">
          <div v-if="view.approval_id === null" class="notice notice-info" data-testid="cancelled-before-send">
            <div>
              <p class="notice-title">Cancelled before sending</p>
              <p>No delivery was released and no message was sent.</p>
            </div>
          </div>

          <template v-else>
            <div v-if="view.terminal" class="notice" :class="finalTone" role="status" data-testid="final-summary" :data-outcome="view.outcome">
              <div>
                <p class="notice-title">
                  <template v-if="view.outcome === 'completed'">Completed</template>
                  <template v-else-if="view.outcome === 'completed_with_failures'">Completed with failures</template>
                  <template v-else>Cancelled</template>
                </p>
                <p>
                  {{ formatCount(view.totals.sent) }} sent, {{ formatCount(view.totals.failed) }} failed,
                  {{ formatCount(skipped) }} skipped, {{ formatCount(view.totals.cancelled) }} cancelled of
                  {{ formatCount(view.totals.approved_total) }} approved deliveries.
                  <template v-if="view.outcome === 'completed_with_failures'">Failed deliveries are listed below.</template>
                  These numbers are final.
                </p>
              </div>
            </div>
            <div v-else-if="view.outcome === 'settling'" class="notice notice-warning" role="status" data-testid="settling">
              <div>
                <p class="notice-title">Cancelled — finishing requests in flight</p>
                <p>
                  No new deliveries start. {{ formatCount(view.totals.in_flight) }} request(s) already in flight are being
                  recorded truthfully; the numbers become final once they finish.
                </p>
              </div>
            </div>

            <div v-if="workerProblem" class="notice notice-warning" role="alert" data-testid="worker-offline">
              <div>
                <p class="notice-title">{{ WORKER_LABEL[view.worker.status] }}</p>
                <p>
                  <template v-if="view.worker.heartbeat_age_seconds !== null">
                    The delivery worker was last seen {{ Math.round(view.worker.heartbeat_age_seconds) }} s ago
                    (threshold {{ view.worker.stale_after_seconds }} s).
                  </template>
                  <template v-else>No delivery worker has reported a heartbeat.</template>
                  Delivery is paused until a worker runs; queued deliveries are kept and nothing is lost.
                </p>
              </div>
            </div>

            <div class="overall">
              <div class="overall-head">
                <span class="overall-label">Overall progress</span>
                <span class="num" data-testid="overall-progress">
                  {{ formatCount(view.processed) }} / {{ formatCount(view.totals.approved_total) }} ({{ progressPct }}%)
                </span>
              </div>
              <div class="bar" role="progressbar" aria-label="Deliveries processed" aria-valuemin="0" :aria-valuemax="100" :aria-valuenow="progressPct">
                <span class="fill" :style="{ width: `${progressPct}%` }" />
              </div>
            </div>

            <dl class="tiles">
              <div class="tile ok"><dt>Sent (accepted)</dt><dd class="num" data-testid="tile-sent">{{ formatCount(view.totals.sent) }}</dd></div>
              <div class="tile bad"><dt>Failed</dt><dd class="num" data-testid="tile-failed">{{ formatCount(view.totals.failed) }}</dd></div>
              <div class="tile"><dt>Skipped</dt><dd class="num" data-testid="tile-skipped">{{ formatCount(skipped) }}</dd></div>
              <div class="tile"><dt>Cancelled</dt><dd class="num" data-testid="tile-cancelled">{{ formatCount(view.totals.cancelled) }}</dd></div>
              <div class="tile"><dt>Pending</dt><dd class="num" data-testid="tile-pending">{{ formatCount(view.remaining) }}</dd></div>
              <div class="tile"><dt>Provider attempts</dt><dd class="num" data-testid="tile-attempts">{{ formatCount(view.attempts.total) }}</dd></div>
              <div class="tile"><dt>Retries</dt><dd class="num" data-testid="tile-retries">{{ formatCount(view.attempts.retries) }}</dd></div>
              <div class="tile"><dt>Attempts / s</dt><dd class="num" data-testid="tile-rate">{{ view.attempts.per_second.toFixed(1) }}</dd></div>
            </dl>

            <div v-if="view.totals.uncertain > 0" class="notice notice-warning" data-testid="uncertain-note">
              <div>
                <p class="notice-title">{{ formatCount(view.totals.uncertain) }} uncertain outcome{{ view.totals.uncertain === 1 ? '' : 's' }}</p>
                <p>
                  An attempt of these deliveries timed out or was interrupted, so the provider may have accepted it without
                  confirming. They are counted under their recorded status (not as sent).
                </p>
              </div>
            </div>

            <div class="channels">
              <ChannelProgress v-for="ch in channelsShown" :key="ch" :channel="ch" :stats="view.channels[ch]!" />
            </div>

            <p class="footnote" data-testid="delivery-explainer">
              “Sent” means the mock provider accepted the request; real-world delivery to a phone or inbox is not tracked.
              Provider attempts ({{ formatCount(view.attempts.total) }}) include retries<template v-if="rateLimited"> and
              {{ formatCount(rateLimited) }} rate-limited (429) answers</template>; a delivery is counted once whatever its
              number of attempts.
            </p>

            <div v-if="reasons.length" class="reasons" data-testid="reason-summary">
              <h3 class="sub">Reasons</h3>
              <ul>
                <li v-for="r in reasons" :key="`${r.channel}-${r.status}-${r.code}`">
                  <span class="num">{{ formatCount(r.count) }}</span>
                  {{ CHANNEL_LABEL[r.channel] }} · {{ DELIVERY_STATUS_LABEL[r.status as keyof typeof DELIVERY_STATUS_LABEL] }} —
                  {{ reasonLabel(r.code) }}
                </li>
              </ul>
            </div>

            <dl class="identity" data-testid="delivery-identity">
              <div><dt>Approval</dt><dd class="mono">{{ view.approval_id.slice(0, 8) }}</dd></div>
              <div><dt>Content version</dt><dd>{{ view.content_version ?? '—' }}</dd></div>
              <div><dt>Segment version</dt><dd>{{ view.segment_version ?? '—' }}</dd></div>
              <div><dt>Campaign version</dt><dd>{{ view.campaign_version }}</dd></div>
              <div v-if="view.send_started_at"><dt>Started</dt><dd>{{ formatDateTime(view.send_started_at) }}</dd></div>
              <div v-if="view.completed_at"><dt>Completed</dt><dd>{{ formatDateTime(view.completed_at) }}</dd></div>
              <div v-if="view.cancelled_at"><dt>Cancelled</dt><dd>{{ formatDateTime(view.cancelled_at) }}</dd></div>
              <div>
                <dt>Worker</dt>
                <dd data-testid="worker-status" :data-status="view.worker.status">{{ WORKER_LABEL[view.worker.status] }}</dd>
              </div>
            </dl>

            <FailureTable v-if="campaignId && (view.processed > 0 || view.totals.retry_scheduled > 0)" :campaign-id="campaignId" :refresh-key="failureKey" />
          </template>

          <div v-if="status === 'sending'" class="actions">
            <button type="button" class="btn btn-secondary danger" :disabled="!canCancelSending" data-testid="cancel-sending" @click="confirmCancel = true">
              Cancel sending…
            </button>
          </div>
        </template>
      </template>
    </div>

    <ConfirmDialog
      v-if="confirmSend && status === 'approved'"
      title="Start sending?"
      confirm-label="Start sending"
      pending-label="Starting…"
      cancel-label="Not now"
      :pending="sending"
      :disabled="!canSend && !sending"
      testid="send-dialog"
      @confirm="doSend"
      @close="confirmSend = false"
    >
      <p v-if="snapshot">
        This releases <strong data-testid="send-dialog-total">{{ formatCount(snapshot.total) }}</strong> deliveries of the
        confirmed approval (content version {{ snapshot.contentVersion }}):
        <span v-for="(c, i) in snapshot.perChannel" :key="c.channel">{{ i ? ', ' : '' }}{{ CHANNEL_LABEL[c.channel] }} {{ formatCount(c.n) }}</span>.
      </p>
      <p>
        This cannot be undone. Messages go to the mock provider only. The worker may start sending before this
        confirmation finishes; you can cancel later, which stops new deliveries but cannot recall requests already in flight.
      </p>
    </ConfirmDialog>

    <ConfirmDialog
      v-if="confirmCancel && status === 'sending'"
      title="Cancel sending?"
      confirm-label="Cancel sending"
      pending-label="Cancelling…"
      cancel-label="Keep sending"
      :pending="cancelling"
      danger
      testid="cancel-dialog"
      @confirm="doCancel"
      @close="confirmCancel = false"
    >
      <p>
        Queued and scheduled deliveries are cancelled immediately. Requests already in flight cannot be recalled: each
        may still reach the provider once, and its real outcome is recorded. Deliveries already sent stay sent.
      </p>
      <p>The campaign cannot be resumed after cancellation.</p>
    </ConfirmDialog>
  </section>
</template>

<style scoped>
.header {
  display: flex;
  flex-wrap: wrap;
  justify-content: space-between;
  align-items: center;
  gap: var(--space-2);
}
.conn {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--space-2);
}
.body {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  min-width: 0;
}
.text {
  font-size: 14px;
  color: var(--c-text-2);
}
.actions {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-3);
  align-items: center;
}
.hint,
.footnote {
  font-size: 13px;
  color: var(--c-muted);
}
.danger {
  color: var(--c-danger);
}
.notice-success {
  background: var(--c-success-soft);
  border-color: #a7f3d0;
  color: var(--c-success);
}
.overall {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.overall-head {
  display: flex;
  flex-wrap: wrap;
  justify-content: space-between;
  gap: var(--space-2);
  font-size: 14px;
}
.overall-label {
  font-weight: 650;
}
.bar {
  height: 14px;
  border-radius: 999px;
  background: var(--c-surface-2);
  border: 1px solid var(--c-border);
  overflow: hidden;
}
.fill {
  display: block;
  height: 100%;
  background: var(--c-primary);
  transition: width 0.3s ease;
}
.tiles {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(120px, 1fr));
  gap: var(--space-3);
  margin: 0;
}
.tile {
  border: 1px solid var(--c-border);
  border-radius: var(--radius-sm);
  padding: var(--space-3);
  min-width: 0;
}
.tile dt {
  font-size: 12px;
  color: var(--c-muted);
}
.tile dd {
  margin: 2px 0 0;
  font-size: 20px;
  font-weight: 700;
}
.tile.ok dd {
  color: var(--c-success);
}
.tile.bad dd {
  color: var(--c-danger);
}
.channels {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(min(280px, 100%), 1fr));
  gap: var(--space-5);
}
.sub {
  font-size: 15px;
  font-weight: 650;
  margin-bottom: var(--space-2);
}
.reasons ul {
  margin: 0;
  padding-left: var(--space-5);
  font-size: 13.5px;
  display: flex;
  flex-direction: column;
  gap: 4px;
  overflow-wrap: anywhere;
}
.identity {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
  gap: var(--space-2) var(--space-4);
  margin: 0;
  font-size: 13px;
}
.identity dt {
  color: var(--c-muted);
}
.identity dd {
  margin: 0;
  overflow-wrap: anywhere;
}
</style>
