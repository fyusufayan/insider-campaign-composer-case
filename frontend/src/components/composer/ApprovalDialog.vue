<script setup lang="ts">
// Review dialog of a prepared (pending) approval. Shows exactly what the
// server froze and every advisory offer/price finding, explains what
// confirming authorizes, and requires explicit acknowledgement before
// Confirm. Closing it decides nothing: the approval stays pending until it
// is confirmed, withdrawn or expires. The server enforces version, binding
// and expiry; this dialog only presents them.
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import type { ApprovalSummary, Campaign } from '@/api/types'
import type { ApiError } from '@/api/client'
import type { Operation } from '@/api/errors'
import ErrorNotice from '@/components/ui/ErrorNotice.vue'
import { formatCount } from '@/composables/format'
import ApprovalSummaryView from './ApprovalSummary.vue'

const props = defineProps<{
  campaign: Campaign
  approval: ApprovalSummary
  remaining: string
  expired: boolean
  confirming: boolean
  withdrawing: boolean
  busy: boolean
  /** The current version shown matches the approval (else: reload first). */
  current: boolean
  error: { op: Operation; error: ApiError } | null
}>()
const emit = defineEmits<{ confirm: []; withdraw: []; close: []; reload: [] }>()

const dialog = ref<HTMLElement | null>(null)
const ackSnapshot = ref(false)
const ackFindings = ref(false)
watch(
  () => props.approval.approval_id,
  () => {
    ackSnapshot.value = false
    ackFindings.value = false
  },
)

const findings = computed(() => props.approval.review?.offer_findings ?? [])
const offerText = computed(() => {
  const o = props.approval.review?.offer
  if (!o) return 'unknown'
  if (o.kind === 'percent_discount') return `${o.value}% discount`
  if (o.kind === 'free_shipping') return 'free shipping'
  return 'no offer'
})
const canConfirm = computed(
  () =>
    props.approval.status === 'pending' &&
    props.current &&
    !props.expired &&
    !props.busy &&
    ackSnapshot.value &&
    (findings.value.length === 0 || ackFindings.value),
)
const confirmHint = computed(() => {
  if (props.expired) return 'The approval has expired and can no longer be confirmed.'
  if (!props.current) return 'The campaign changed; reload before confirming.'
  if (!ackSnapshot.value || (findings.value.length > 0 && !ackFindings.value)) return 'Tick the acknowledgement above to confirm.'
  return ''
})

function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape' && !props.confirming && !props.withdrawing) emit('close')
}
onMounted(async () => {
  document.addEventListener('keydown', onKey)
  await nextTick()
  dialog.value?.focus()
})
onBeforeUnmount(() => document.removeEventListener('keydown', onKey))
</script>

<template>
  <div class="backdrop" data-testid="approval-dialog-backdrop">
    <div
      ref="dialog"
      class="dialog card"
      role="dialog"
      aria-modal="true"
      aria-labelledby="approval-dialog-title"
      tabindex="-1"
      data-testid="approval-dialog"
    >
      <header class="card-header head">
        <div>
          <p class="card-eyebrow">Review approval</p>
          <h2 id="approval-dialog-title" class="title">{{ campaign.name }}</h2>
        </div>
        <button
          type="button"
          class="btn btn-ghost btn-sm"
          :disabled="confirming || withdrawing"
          data-testid="approval-close"
          @click="emit('close')"
        >
          Close without confirming
        </button>
      </header>

      <div class="card-body body">
        <div v-if="expired" class="notice notice-danger" role="alert" data-testid="approval-expired-banner">
          <div>
            <p class="notice-title">This approval has expired</p>
            <p>It can no longer be confirmed. Nothing was sent. Close this dialog and prepare a new approval.</p>
          </div>
        </div>
        <div v-else class="notice notice-info" role="note" data-testid="approval-explainer">
          <div>
            <p class="notice-title">What confirming means</p>
            <p>
              You authorize exactly the content version and the frozen recipient snapshot below
              ({{ formatCount(approval.snapshot_count) }} deliveries). Users who start matching the rule later are never
              added. At delivery time consent and contact details are checked again, so recipients who opt out or lose
              their contact are skipped. Confirming does not send anything.
            </p>
            <p class="countdown" data-testid="approval-countdown">
              Confirm within <strong>{{ remaining }}</strong>; after that the snapshot is released.
            </p>
          </div>
        </div>

        <ApprovalSummaryView :approval="approval" :remaining="remaining" />

        <section class="findings" :class="{ has: findings.length }" data-testid="approval-offer-findings">
          <h3 class="findings-title">
            Offer and price review
            <span class="badge" :class="findings.length ? 'badge-warning' : 'badge-neutral'">
              {{ findings.length ? `${findings.length} advisory ${findings.length === 1 ? 'finding' : 'findings'}` : 'no findings' }}
            </span>
          </h3>
          <p class="findings-intro">
            Authorized by the content brief: <strong data-testid="approval-offer">{{ offerText }}</strong>. The copy was
            re-checked against the current brief when this approval was read. These findings are advisory — they do not
            block approval — and the check is keyword-based: it cannot guarantee that offers, prices or claims are
            accurate or legally compliant. You are responsible for that review.
          </p>
          <ul v-if="findings.length" class="finding-list">
            <li v-for="(f, i) in findings" :key="i" :data-testid="`approval-finding-${i}`">
              <span class="badge badge-warning">{{ f.code === 'unauthorized_claim' ? 'Claim' : 'Offer' }}</span>
              <span class="mono path">{{ f.path }}</span>
              <span>{{ f.message }}</span>
            </li>
          </ul>
        </section>

        <fieldset class="acks" :disabled="approval.status !== 'pending' || expired">
          <legend class="sr-only">Acknowledgement</legend>
          <label>
            <input v-model="ackSnapshot" type="checkbox" data-testid="ack-snapshot" />
            I reviewed the frozen content and the recipient counts above and authorize this snapshot.
          </label>
          <label v-if="findings.length">
            <input v-model="ackFindings" type="checkbox" data-testid="ack-findings" />
            I reviewed all {{ findings.length }} offer/price {{ findings.length === 1 ? 'finding' : 'findings' }} and
            accept the copy as written.
          </label>
        </fieldset>

        <ErrorNotice
          v-if="error"
          :error="error.error"
          :op="error.op"
          :busy="busy"
          @reload="emit('reload')"
          @retry="error.op === 'withdrawApproval' ? emit('withdraw') : emit('confirm')"
        />

        <div class="actions">
          <button
            type="button"
            class="btn btn-primary"
            :disabled="!canConfirm"
            data-testid="approval-confirm"
            @click="emit('confirm')"
          >
            <span v-if="confirming" class="spinner" aria-hidden="true" />
            {{ confirming ? 'Confirming…' : 'Confirm approval' }}
          </button>
          <button
            type="button"
            class="btn btn-secondary danger"
            :disabled="busy || approval.status !== 'pending' || !current"
            data-testid="approval-withdraw"
            @click="emit('withdraw')"
          >
            {{ withdrawing ? 'Withdrawing…' : 'Withdraw approval' }}
          </button>
          <p v-if="confirmHint" class="hint" data-testid="approval-confirm-hint">{{ confirmHint }}</p>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.backdrop {
  position: fixed;
  inset: 0;
  background: rgb(15 23 42 / 0.45);
  display: flex;
  align-items: flex-start;
  justify-content: center;
  padding: var(--space-6) var(--space-4);
  overflow-y: auto;
  z-index: 50;
}
.dialog {
  width: min(960px, 100%);
  outline: none;
}
.head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: var(--space-3);
  flex-wrap: wrap;
}
.title {
  font-size: 20px;
  font-weight: 700;
  overflow-wrap: anywhere;
}
.body {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}
.countdown {
  margin-top: var(--space-2);
}
.findings {
  border: 1px solid var(--c-border, #e2e8f0);
  border-radius: 8px;
  padding: var(--space-3) var(--space-4);
}
.findings.has {
  border-color: #f59e0b;
  background: #fffbeb;
}
.findings-title {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: 15px;
  font-weight: 650;
}
.findings-intro {
  margin-top: var(--space-2);
  font-size: 13.5px;
}
.finding-list {
  margin: var(--space-2) 0 0;
  padding: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.finding-list li {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  align-items: baseline;
  overflow-wrap: anywhere;
}
.path {
  color: var(--c-muted);
}
.acks {
  border: 0;
  padding: 0;
  margin: 0;
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
.acks label {
  display: flex;
  gap: var(--space-2);
  align-items: flex-start;
}
.actions {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-3);
  align-items: center;
}
.danger {
  color: #b91c1c;
}
.hint {
  font-size: 13px;
  color: var(--c-muted);
  flex-basis: 100%;
}
.sr-only {
  position: absolute;
  width: 1px;
  height: 1px;
  overflow: hidden;
  clip: rect(0 0 0 0);
}
@media (max-width: 600px) {
  .backdrop {
    padding: 0;
  }
  .dialog {
    min-height: 100%;
    border-radius: 0;
  }
  .actions .btn {
    width: 100%;
  }
}
</style>
