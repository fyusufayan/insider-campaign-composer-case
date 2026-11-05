<script setup lang="ts">
// Step 5: approval. Prepare freezes the audience and content into a pending
// approval (server); the dialog reviews and confirms or withdraws it; an
// approved campaign shows a read-only summary; sending starts from step 6
// (DeliveryPanel), never from here. All state lives in the campaign store.
import { computed, ref, watch } from 'vue'
import { storeToRefs } from 'pinia'
import { useCampaignStore } from '@/stores/campaign'
import { formatRemaining, useCountdown } from '@/composables/useCountdown'
import ErrorNotice from '@/components/ui/ErrorNotice.vue'
import ApprovalDialog from './ApprovalDialog.vue'
import ApprovalSummaryView from './ApprovalSummary.vue'

const store = useCampaignStore()
const {
  campaign,
  approval,
  approvalLoading,
  approvalError,
  approvalActionError,
  preparing,
  confirming,
  withdrawing,
  approvalDialogOpen,
  approvalNotice,
  serverOffsetMs,
  busy,
  approvalCurrent,
  liveApproval,
  prepareBlockedReason,
  canPrepare,
} = storeToRefs(store)

const expiresAt = computed(() => liveApproval.value?.expires_at ?? null)
const { remainingMs, elapsed } = useCountdown(expiresAt, serverOffsetMs, () => store.approvalDeadlinePassed())
const remaining = computed(() => formatRemaining(remainingMs.value))

const status = computed(() => campaign.value?.status ?? null)
const pending = computed(() => liveApproval.value?.status === 'pending' ? liveApproval.value : null)
const approved = computed(() => liveApproval.value?.status === 'approved' ? liveApproval.value : null)
const finished = computed(
  () => !!status.value && ['sending', 'completed', 'completed_with_failures', 'cancelled'].includes(status.value),
)

// Withdrawing a confirmed approval takes an explicit second step.
const confirmWithdraw = ref(false)
watch(
  () => liveApproval.value?.approval_id,
  () => (confirmWithdraw.value = false),
)

const dialogError = computed(() =>
  approvalActionError.value && approvalActionError.value.op !== 'prepareApproval' ? approvalActionError.value : null,
)
const panelError = computed(() => {
  const e = approvalActionError.value
  if (!e) return null
  if (e.op === 'prepareApproval') return e
  return approvalDialogOpen.value ? null : e
})

const NOTICE: Record<string, { title: string; text: string; tone: string }> = {
  expired: {
    title: 'Approval expired — review again',
    text: 'The approval passed its time limit before it was used. Its recipient snapshot was released and nothing was sent. Prepare a new approval to continue.',
    tone: 'notice-warning',
  },
  withdrawn: {
    title: 'Approval withdrawn',
    text: 'The approval was withdrawn and its recipient snapshot released. Nothing was sent. You can edit the campaign or prepare a new approval.',
    tone: 'notice-info',
  },
  replaced: {
    title: 'Approval replaced',
    text: 'This approval is no longer current (it was replaced elsewhere). Review the current state below.',
    tone: 'notice-warning',
  },
  confirmed: {
    title: 'Approval confirmed',
    text: 'The campaign is approved for exactly the frozen content and recipient snapshot. Nothing has been sent.',
    tone: 'notice-info',
  },
}
const notice = computed(() => (approvalNotice.value ? NOTICE[approvalNotice.value] : null))
</script>

<template>
  <section class="card approval-panel" data-testid="approval-panel">
    <div class="card-header">
      <p class="card-eyebrow"><span class="step-num">5</span> Review &amp; approval</p>
    </div>
    <div class="card-body body">
      <div v-if="notice" class="notice" :class="notice.tone" role="status" data-testid="approval-notice" :data-kind="approvalNotice">
        <div>
          <p class="notice-title">{{ notice.title }}</p>
          <p>{{ notice.text }}</p>
        </div>
        <button type="button" class="btn btn-ghost btn-sm" @click="store.dismissApprovalNotice()">Dismiss</button>
      </div>

      <ErrorNotice
        v-if="panelError"
        :error="panelError.error"
        :op="panelError.op"
        :busy="busy"
        data-testid="approval-action-error"
        @retry="panelError.op === 'prepareApproval' ? store.prepareApproval() : store.reload()"
        @reload="store.reload()"
      />
      <ErrorNotice
        v-if="approvalError"
        :error="approvalError"
        op="approval"
        tone="warning"
        retry-label="Load approval again"
        @retry="store.refreshApproval()"
        @reload="store.reload()"
      />

      <!-- Ready to prepare -->
      <template v-if="status === 'content_ready'">
        <p class="text">
          Preparing an approval freezes the current content version and the exact recipients who are eligible right
          now (matching the rule, with contact and consent) for the selected channels. You review that snapshot before
          confirming. Nothing is sent.
        </p>
        <div class="actions">
          <button
            type="button"
            class="btn btn-primary"
            :disabled="!canPrepare"
            data-testid="prepare-approval"
            @click="store.prepareApproval()"
          >
            <span v-if="preparing" class="spinner" aria-hidden="true" />
            {{ preparing ? 'Preparing…' : 'Prepare approval' }}
          </button>
          <span v-if="prepareBlockedReason && !preparing" class="hint" data-testid="prepare-blocked">{{ prepareBlockedReason }}</span>
        </div>
      </template>

      <!-- Pending: resume the review -->
      <template v-else-if="status === 'pending_approval'">
        <p v-if="approvalLoading && !pending" class="hint" role="status"><span class="spinner" aria-hidden="true" /> Loading the approval…</p>
        <div v-if="pending" class="pending" data-testid="approval-pending">
          <p>
            An approval of <strong>{{ pending.snapshot_count }}</strong> frozen deliveries is waiting for your confirmation.
            <span v-if="!elapsed">Confirm within <strong data-testid="pending-countdown">{{ remaining }}</strong>.</span>
            <span v-else data-testid="pending-expired">Its time limit has passed; checking with the server…</span>
          </p>
          <div class="actions">
            <button type="button" class="btn btn-primary" :disabled="busy" data-testid="open-approval-dialog" @click="store.openApprovalDialog()">
              Review &amp; confirm
            </button>
            <button type="button" class="btn btn-secondary" :disabled="busy" data-testid="panel-withdraw" @click="store.withdrawApproval()">
              {{ withdrawing ? 'Withdrawing…' : 'Withdraw' }}
            </button>
          </div>
        </div>
      </template>

      <!-- Approved: read-only summary -->
      <template v-else-if="status === 'approved'">
        <p v-if="approvalLoading && !approved" class="hint" role="status"><span class="spinner" aria-hidden="true" /> Loading the approval…</p>
        <div v-if="approved" class="approved" data-testid="approval-approved">
          <div class="notice notice-info" role="note">
            <div>
              <p class="notice-title">Approved — not sent</p>
              <p>
                Valid for <strong data-testid="approved-countdown">{{ remaining }}</strong>. No messages have been sent:
                start sending explicitly in step 6 below. Consent and contact will be re-checked for every delivery.
              </p>
            </div>
          </div>
          <ApprovalSummaryView :approval="approved" :remaining="remaining" />
          <div class="actions">
            <template v-if="!confirmWithdraw">
              <button type="button" class="btn btn-secondary" :disabled="busy" data-testid="approved-withdraw" @click="confirmWithdraw = true">
                Withdraw approval…
              </button>
            </template>
            <template v-else>
              <span class="warn" data-testid="approved-withdraw-confirm-text">
                Withdraw the confirmed approval? Its snapshot is released and you will need to prepare and confirm again.
              </span>
              <button type="button" class="btn btn-secondary danger" :disabled="busy" data-testid="approved-withdraw-confirm" @click="store.withdrawApproval()">
                {{ withdrawing ? 'Withdrawing…' : 'Yes, withdraw' }}
              </button>
              <button type="button" class="btn btn-ghost" :disabled="busy" @click="confirmWithdraw = false">Keep approval</button>
            </template>
          </div>
        </div>
      </template>

      <p v-else-if="finished" class="hint" data-testid="approval-finished">This campaign can no longer be approved.</p>
      <p v-else class="hint" data-testid="approval-not-ready">Generate or write content to prepare an approval.</p>
    </div>

    <ApprovalDialog
      v-if="approvalDialogOpen && pending && campaign"
      :campaign="campaign"
      :approval="pending"
      :remaining="remaining"
      :expired="elapsed"
      :confirming="confirming"
      :withdrawing="withdrawing"
      :busy="busy"
      :current="approvalCurrent"
      :error="dialogError"
      @confirm="store.confirmApproval()"
      @withdraw="store.withdrawApproval()"
      @close="store.closeApprovalDialog()"
      @reload="store.reload()"
    />
  </section>
</template>

<style scoped>
.body {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
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
.hint {
  font-size: 13px;
  color: var(--c-muted);
}
.pending,
.approved {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.warn {
  flex-basis: 100%;
  font-size: 13.5px;
}
.danger {
  color: #b91c1c;
}
</style>
