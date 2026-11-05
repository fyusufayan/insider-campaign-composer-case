// The active composer campaign (ARCHITECTURE §14: one Pinia store for the
// active campaign; other server state is fetched per page).
//
// Async correctness rules:
//   1. Epoch: open()/close() abort every in-flight request and bump `epoch`;
//      a response from an older epoch (another campaign, or after the page
//      unmounted) is dropped.
//   2. Monotonic campaign: a campaign response is applied only if it is the
//      same campaign and its `version` is ≥ the local one, so an older
//      response can never roll back a newer state.
//   3. Audience binding: statistics are shown only for the campaign version
//      they were computed for (`campaign_version` of GET /audience, or the
//      campaign returned with POST /interpret). A response for an older
//      version is dropped; one for a newer version means the campaign
//      changed elsewhere and is surfaced as a stale notice (never applied
//      over local state). Each refresh aborts the previous one.
//   4. One mutation at a time: interpret, channel updates, content generation
//      and content saves all bump the campaign version, so while one is
//      pending the others are refused and the UI disables their controls.
//      Stale writes are never retried automatically: a 409 keeps the user's
//      selection (or content draft) aside for an explicit re-apply after
//      reloading.
//   5. Content binding: the current content (GET /content) carries the
//      campaign_version its validation was computed for (validation depends
//      on the selected channels); it is applied only for the version shown,
//      like the audience. Draft validations are bound to the draft revision
//      and the campaign version they were computed for.
//   6. Generation stream: events are applied only while the epoch and the
//      request are current; streamed deltas are an unvalidated preview kept
//      apart from the editor draft. Only the "validated" event (sent after
//      the commit) changes the campaign and its content. Any failure,
//      interruption or cancellation reloads the campaign and its content,
//      because a commit may have happened just before the connection ended.
//   7. Approval (CP15): the approval summary is applied only together with
//      the campaign returned in the same response (same version), so the
//      dialog never shows an approval for another campaign state. Prepare,
//      confirm and withdraw are version-guarded mutations like the others
//      (one at a time, never retried automatically; a 409 is shown with a
//      reload path). The expiry countdown is display only: when it elapses
//      the store re-reads the approval (the server performs the expiry) and
//      Confirm is disabled; the server's decision is authoritative.
//   8. Send / cancel (CP17B): explicit, version-guarded mutations, one at a
//      time (a second click while one is pending is ignored). Nothing ever
//      sends implicitly (opening, refreshing or approving never calls
//      /send). already_started is a success. A lost response (network
//      error) or an invalid_state is never guessed at: the campaign is
//      re-read with a fresh GET. Delivery statistics are server state of the
//      dashboard (useCampaignEvents), not kept here; when they report a newer
//      campaign version (completion, cancellation elsewhere) the campaign is
//      re-read so the status shown follows the server.

import { defineStore } from 'pinia'
import { computed, ref, shallowRef } from 'vue'
import { ApiError, api, isApiError } from '@/api/client'
import type {
  ApprovalResponse,
  ApprovalSummary,
  CancelResponse,
  SendResponse,
  AudienceStats,
  Campaign,
  CampaignStatus,
  Channel,
  ContentDraft,
  ContentField,
  ContentSaved,
  ContentValidation,
  CurrentContent,
  GenerationEvent,
  GenerationPhase,
} from '@/api/types'
import { CHANNEL_FIELDS, CHANNELS, CONTENT_FIELDS } from '@/api/types'

const AUDIENCE_STATUSES: readonly CampaignStatus[] = ['segmented', 'content_ready', 'pending_approval', 'approved']
const INTERPRET_STATUSES: readonly CampaignStatus[] = ['draft', 'segmented', 'content_ready']
const CHANNEL_STATUSES: readonly CampaignStatus[] = ['segmented', 'content_ready']
/** Statuses in which content may be generated or saved (pending/approved: Slice 3). */
const CONTENT_EDIT_STATUSES: readonly CampaignStatus[] = ['segmented', 'content_ready']

/** Bound on the live preview per field (characters). */
const MAX_PREVIEW_CHARS = 20_000

export type GenerationUiPhase = 'idle' | 'connecting' | GenerationPhase | 'done' | 'failed' | 'cancelled'

/** State of the current (or last) generation, for display. */
export interface GenerationView {
  phase: GenerationUiPhase
  /** Model request number (1-based); 0 before the first. */
  attempt: number
  reason: 'initial' | 'truncation_retry' | 'repair' | null
  /** Field problems of the rejected attempt (phase repairing). */
  problems: number
  fields: ContentField[]
  /** Unvalidated, unsaved preview of the current attempt. */
  preview: Partial<Record<ContentField, string>>
  error: ApiError | null
  notes: string[]
}

export type Draft = Record<ContentField, string>

export type ApprovalAction = 'prepareApproval' | 'confirmApproval' | 'withdrawApproval'
/** How the approval shown last ended, or that it was just confirmed. */
export type ApprovalNotice = 'expired' | 'withdrawn' | 'replaced' | 'confirmed'

export type DeliveryAction = 'send' | 'cancel'

/** Outcome of the last send/cancel shown as a banner (counts only). */
export type DeliveryNotice =
  | { kind: 'started'; released: number }
  | { kind: 'already_started' }
  | { kind: 'cancelled'; cancelled: number; inFlight: number; releasedHeld: number }
  | { kind: 'reconciled'; op: DeliveryAction }

const DELIVERY_STATUSES: readonly CampaignStatus[] = ['approved', 'sending', 'completed', 'completed_with_failures', 'cancelled']


const IDLE_GENERATION: GenerationView = {
  phase: 'idle',
  attempt: 0,
  reason: null,
  problems: 0,
  fields: [],
  preview: {},
  error: null,
  notes: [],
}

function emptyDraft(): Draft {
  return { sms_body: '', email_subject: '', email_body: '' }
}

/** Fields of the selected channels, in canonical order. */
export function fieldsFor(chs: readonly Channel[]): ContentField[] {
  return CONTENT_FIELDS.filter((f) => canonicalChannels(chs).some((c) => CHANNEL_FIELDS[c].includes(f)))
}

export function canonicalChannels(chs: readonly Channel[]): Channel[] {
  return CHANNELS.filter((c) => chs.includes(c))
}

function sameChannels(a: readonly Channel[], b: readonly Channel[]): boolean {
  const ca = canonicalChannels(a)
  const cb = canonicalChannels(b)
  return ca.length === cb.length && ca.every((c, i) => c === cb[i])
}

function toApiError(e: unknown): ApiError {
  if (isApiError(e)) return e
  return new ApiError({ kind: 'invalid_response', code: 'client_error', message: 'An unexpected error occurred.' })
}

export const useCampaignStore = defineStore('campaign', () => {
  // ---- state ----
  const campaignId = ref<string | null>(null)
  const campaign = shallowRef<Campaign | null>(null)
  const loading = ref(false)
  const loadError = shallowRef<ApiError | null>(null)

  const interpreting = ref(false)
  const interpretError = shallowRef<ApiError | null>(null)

  const audience = shallowRef<AudienceStats | null>(null)
  /** Campaign version the displayed statistics belong to. */
  const audienceForVersion = ref<number | null>(null)
  const audienceChannels = shallowRef<Channel[]>([])
  const audienceLoading = ref(false)
  const audienceError = shallowRef<ApiError | null>(null)
  /** The audience error happened right after a successful (saved) interpretation. */
  const audienceErrorAfterInterpret = ref(false)

  const savingChannels = ref(false)
  /** Selection being saved (shown optimistically while the PUT is in flight). */
  const pendingChannels = shallowRef<Channel[] | null>(null)
  const channelsError = shallowRef<ApiError | null>(null)
  /** Selection rejected by a version conflict, kept for an explicit re-apply. */
  const unsavedChannels = shallowRef<Channel[] | null>(null)

  /** The server reported a newer campaign version than the one shown. */
  const staleNotice = ref(false)

  // ---- content ----
  /** Current content view (bound to its campaign_version). */
  const content = shallowRef<CurrentContent | null>(null)
  const contentLoading = ref(false)
  const contentError = shallowRef<ApiError | null>(null)
  /** Editor draft (all fields; only the selected channels' fields are sent). */
  const draft = ref<Draft>(emptyDraft())
  /** Bumped on every draft change; validations are bound to it. */
  const draftRevision = ref(0)
  /** Content version id the draft started from (null: none). */
  const draftBaseId = ref<number | null>(null)
  const draftValidation = shallowRef<ContentValidation | null>(null)
  const draftValidationFor = shallowRef<{ revision: number; version: number } | null>(null)
  const validatingDraft = ref(false)
  const draftValidationError = shallowRef<ApiError | null>(null)
  const savingContent = ref(false)
  const saveError = shallowRef<ApiError | null>(null)
  /** Outcome of the last save: a new version, or unchanged text (no version). */
  const saveNotice = ref<'saved' | 'unchanged' | null>(null)
  const generation = shallowRef<GenerationView>(IDLE_GENERATION)

  // ---- approval (CP15) ----
  const approval = shallowRef<ApprovalSummary | null>(null)
  const approvalLoading = ref(false)
  /** Reading the approval failed. */
  const approvalError = shallowRef<ApiError | null>(null)
  /** The last prepare/confirm/withdraw failed (kept until reload or the next action). */
  const approvalActionError = shallowRef<{ op: ApprovalAction; error: ApiError } | null>(null)
  const preparing = ref(false)
  const confirming = ref(false)
  const withdrawing = ref(false)
  const approvalDialogOpen = ref(false)
  /** What happened to the approval shown last (banner). */
  const approvalNotice = ref<ApprovalNotice | null>(null)
  /** server clock − client clock (ms), from the last approval response. */
  const serverOffsetMs = ref(0)

  // ---- delivery (CP17B) ----
  const sending = ref(false)
  const cancelling = ref(false)
  const deliveryActionError = shallowRef<{ op: DeliveryAction; error: ApiError } | null>(null)
  const deliveryNotice = shallowRef<DeliveryNotice | null>(null)
  const lastCancel = shallowRef<CancelResponse | null>(null)
  const lastSend = shallowRef<SendResponse | null>(null)

  let epoch = 0
  const controllers = new Set<AbortController>()
  let audienceController: AbortController | null = null
  let contentController: AbortController | null = null
  let validateController: AbortController | null = null
  let generationController: AbortController | null = null
  let generationCancelled = false
  let approvalController: AbortController | null = null

  // ---- derived ----
  const generating = computed(() => !['idle', 'done', 'failed', 'cancelled'].includes(generation.value.phase))
  const approvalBusy = computed(() => preparing.value || confirming.value || withdrawing.value)
  const deliveryBusy = computed(() => sending.value || cancelling.value)
  const busy = computed(
    () =>
      interpreting.value ||
      savingChannels.value ||
      generating.value ||
      savingContent.value ||
      approvalBusy.value ||
      deliveryBusy.value,
  )
  const status = computed(() => campaign.value?.status ?? null)
  const canInterpret = computed(() => !!status.value && INTERPRET_STATUSES.includes(status.value))
  const canEditChannels = computed(() => !!status.value && CHANNEL_STATUSES.includes(status.value))
  const hasAudienceStep = computed(() => !!status.value && AUDIENCE_STATUSES.includes(status.value))
  const selectedChannels = computed<Channel[]>(() => pendingChannels.value ?? campaign.value?.channels ?? [])
  const conflict = computed(() => channelsError.value?.code === 'version_conflict')
  /** Statistics match the campaign version and channels currently shown. */
  const audienceCurrent = computed(
    () =>
      !!campaign.value &&
      audience.value !== null &&
      audienceForVersion.value === campaign.value.version &&
      sameChannels(audienceChannels.value, campaign.value.channels),
  )

  /** Content view matches the campaign version shown. */
  const contentCurrent = computed(
    () => !!campaign.value && !!content.value && content.value.campaign_version === campaign.value.version,
  )
  const savedVersion = computed(() => content.value?.content ?? null)
  /** Fields edited for the current selection. */
  const editableFields = computed<ContentField[]>(() => fieldsFor(campaign.value?.channels ?? []))
  function savedText(f: ContentField): string {
    return savedVersion.value?.[f] ?? ''
  }
  /** The draft differs from the saved version in a field of a selected channel. */
  const dirty = computed(() => editableFields.value.some((f) => draft.value[f] !== savedText(f)))
  const hasContentStep = computed(() => !!campaign.value?.segment_rule && hasAudienceStep.value)
  const canEditContent = computed(
    () => !!status.value && CONTENT_EDIT_STATUSES.includes(status.value) && editableFields.value.length > 0,
  )
  const canGenerate = computed(() => canEditContent.value && !dirty.value)
  /** Validation to display: the draft's (when current) or the saved version's. */
  const shownValidation = computed<ContentValidation | null>(() => {
    if (dirty.value) {
      const f = draftValidationFor.value
      const current = f && f.revision === draftRevision.value && f.version === campaign.value?.version
      return current ? draftValidation.value : null
    }
    return contentCurrent.value ? (content.value?.validation ?? null) : null
  })

  /** The approval shown belongs to the campaign version shown. */
  const approvalCurrent = computed(
    () =>
      !!campaign.value &&
      !!approval.value &&
      approval.value.campaign_id === campaign.value.id &&
      approval.value.campaign_version === campaign.value.version,
  )
  /** The campaign's live approval, as currently shown. */
  const liveApproval = computed<ApprovalSummary | null>(() => {
    const a = approval.value
    const c = campaign.value
    if (!a || !c || !approvalCurrent.value || c.active_approval_id !== a.approval_id) return null
    return a.status === 'pending' || a.status === 'approved' ? a : null
  })
  /** Why Prepare Approval is unavailable (null: it is available). */
  const prepareBlockedReason = computed<string | null>(() => {
    const c = campaign.value
    if (!c) return 'Loading…'
    if (c.status !== 'content_ready') {
      if (c.status === 'pending_approval') return 'An approval is already prepared.'
      if (c.status === 'approved') return 'The campaign is approved. Withdraw the approval to prepare a new one.'
      return 'Write and save content first.'
    }
    if (c.channels.length === 0) return 'Select at least one channel.'
    if (dirty.value) return 'Save or discard your content edits first.'
    if (!contentCurrent.value) return 'Loading the current content…'
    if (!content.value?.validation?.valid) return 'Fix the content errors first.'
    return null
  })
  const canPrepare = computed(() => !busy.value && prepareBlockedReason.value === null)

  /** The delivery step is shown (approved, sending or finished). */
  const hasDeliveryStep = computed(() => !!status.value && DELIVERY_STATUSES.includes(status.value))
  /** Why Start sending is unavailable (null: it is available). */
  const sendBlockedReason = computed<string | null>(() => {
    const c = campaign.value
    if (!c) return 'Loading…'
    if (c.status !== 'approved') return 'Only an approved campaign can be sent.'
    const a = liveApproval.value
    if (!a) return approvalLoading.value ? 'Loading the approval…' : 'The confirmed approval is not loaded; reload the campaign.'
    if (a.status !== 'approved') return 'The approval is not confirmed.'
    if (Date.parse(a.expires_at) <= Date.now() + serverOffsetMs.value) return 'The approval has expired.'
    if (staleNotice.value) return 'A newer version exists; reload first.'
    return null
  })
  const canSend = computed(() => !busy.value && sendBlockedReason.value === null)
  const canCancelSending = computed(() => !busy.value && status.value === 'sending')

  // ---- helpers ----
  function track(): { controller: AbortController; myEpoch: number } {
    const controller = new AbortController()
    controllers.add(controller)
    return { controller, myEpoch: epoch }
  }

  function untrack(controller: AbortController) {
    controllers.delete(controller)
  }

  function abortAll() {
    for (const c of controllers) c.abort()
    controllers.clear()
    audienceController = null
    contentController = null
    validateController = null
    generationController = null
    approvalController = null
  }

  function resetState() {
    campaign.value = null
    loading.value = false
    loadError.value = null
    interpreting.value = false
    interpretError.value = null
    audience.value = null
    audienceForVersion.value = null
    audienceChannels.value = []
    audienceLoading.value = false
    audienceError.value = null
    audienceErrorAfterInterpret.value = false
    savingChannels.value = false
    pendingChannels.value = null
    channelsError.value = null
    unsavedChannels.value = null
    staleNotice.value = false
    content.value = null
    contentLoading.value = false
    contentError.value = null
    draft.value = emptyDraft()
    draftRevision.value++
    draftBaseId.value = null
    draftValidation.value = null
    draftValidationFor.value = null
    validatingDraft.value = false
    draftValidationError.value = null
    savingContent.value = false
    saveError.value = null
    saveNotice.value = null
    generation.value = IDLE_GENERATION
    generationCancelled = false
    approval.value = null
    approvalLoading.value = false
    approvalError.value = null
    approvalActionError.value = null
    preparing.value = false
    confirming.value = false
    withdrawing.value = false
    approvalDialogOpen.value = false
    approvalNotice.value = null
    serverOffsetMs.value = 0
    sending.value = false
    cancelling.value = false
    deliveryActionError.value = null
    deliveryNotice.value = null
    lastCancel.value = null
    lastSend.value = null
  }

  /** Replaces the draft with the saved version's text (discarding edits). */
  function resetDraft() {
    const v = content.value?.content ?? null
    draft.value = {
      sms_body: v?.sms_body ?? '',
      email_subject: v?.email_subject ?? '',
      email_body: v?.email_body ?? '',
    }
    draftBaseId.value = v?.id ?? null
    draftRevision.value++
    draftValidation.value = null
    draftValidationFor.value = null
    draftValidationError.value = null
  }

  /**
   * Applies a content view for the campaign version shown. A clean draft
   * follows the new saved version; a dirty draft (unsaved edits) is kept.
   * Returns false if the view is for another campaign or version.
   */
  function applyContent(view: CurrentContent, opts: { resetDraft?: boolean } = {}): boolean {
    const cur = campaign.value
    if (!cur || view.campaign_id !== cur.id) return false
    if (view.campaign_version < cur.version) return false
    if (view.campaign_version > cur.version) {
      staleNotice.value = true
      return false
    }
    const wasDirty = dirty.value
    content.value = view
    contentError.value = null
    if (opts.resetDraft || !wasDirty) resetDraft()
    return true
  }

  /** The draft as sent: selected channels' fields, null for the others. */
  function draftPayload(): ContentDraft {
    const fields = editableFields.value
    const v = (f: ContentField) => (fields.includes(f) ? draft.value[f] : null)
    return { sms_body: v('sms_body'), email_subject: v('email_subject'), email_body: v('email_body') }
  }

  /** Applies a campaign response unless it would roll back newer state. */
  function applyCampaign(next: Campaign): boolean {
    if (next.id !== campaignId.value) return false
    const cur = campaign.value
    if (cur && next.version < cur.version) return false
    campaign.value = next
    return true
  }

  function setAudience(stats: AudienceStats, version: number, channels: Channel[]) {
    audience.value = stats
    audienceForVersion.value = version
    audienceChannels.value = canonicalChannels(channels)
    audienceError.value = null
    audienceErrorAfterInterpret.value = false
  }

  // ---- actions ----

  /** Opens a campaign by id (route entry, refresh, direct navigation). */
  async function open(id: string): Promise<void> {
    abortAll()
    epoch++
    resetState()
    campaignId.value = id
    await fetchCampaign({ refreshAudience: true, restoreDialog: true })
  }

  /** Leaves the campaign: aborts pending requests and drops their results. */
  function close(): void {
    abortAll()
    epoch++
    resetState()
    campaignId.value = null
  }

  /** Seeds the store with a campaign just created (avoids a second GET). */
  function adopt(c: Campaign): void {
    abortAll()
    epoch++
    resetState()
    campaignId.value = c.id
    campaign.value = c
  }

  async function fetchCampaign(opts: { refreshAudience: boolean; restoreDialog?: boolean }): Promise<void> {
    const id = campaignId.value
    if (!id) return
    const { controller, myEpoch } = track()
    loading.value = true
    loadError.value = null
    try {
      const c = await api.getCampaign(id, controller.signal)
      if (myEpoch !== epoch) return
      // A reload must accept the server state even if it is "older" than a
      // local optimistic view; versions only grow server-side, so ≥ holds.
      applyCampaign(c)
      staleNotice.value = false
      if (opts.refreshAudience && AUDIENCE_STATUSES.includes(c.status)) {
        void refreshAudience()
      }
      if (c.segment_rule) void refreshContent()
      syncApproval(c, { restoreDialog: opts.restoreDialog === true })
    } catch (e) {
      const err = toApiError(e)
      if (err.isAbort || myEpoch !== epoch) return
      loadError.value = err
    } finally {
      untrack(controller)
      if (myEpoch === epoch) loading.value = false
    }
  }

  /** Reloads the latest campaign after a conflict or a stale notice. */
  async function reload(): Promise<void> {
    if (busy.value) return
    channelsError.value = null
    interpretError.value = null
    saveError.value = null
    approvalActionError.value = null
    deliveryActionError.value = null
    if (generation.value.phase === 'failed') generation.value = IDLE_GENERATION
    await fetchCampaign({ refreshAudience: true })
  }

  /** POST /interpret. Refused while another mutation is pending. */
  async function interpret(): Promise<void> {
    const id = campaignId.value
    if (!id || busy.value || !canInterpret.value) return
    // The audience shown belongs to the pre-interpretation version.
    audienceController?.abort()
    audienceLoading.value = false
    const { controller, myEpoch } = track()
    interpreting.value = true
    interpretError.value = null
    try {
      const res = await api.interpret(id, controller.signal)
      if (myEpoch !== epoch) return
      if (!applyCampaign(res.campaign)) return
      // A new interpretation replaces the rule: drop any conflict state that
      // referred to the previous version, but keep the user's unsaved choice.
      channelsError.value = null
      void refreshContent()
      if (res.audience) {
        setAudience(res.audience, res.campaign.version, res.campaign.channels)
      } else {
        audience.value = null
        audienceForVersion.value = null
        audienceError.value = res.audience_error
          ? new ApiError({
              kind: 'http',
              code: res.audience_error.code,
              message: res.audience_error.message,
              retryable: res.audience_error.retryable,
            })
          : new ApiError({
              kind: 'invalid_response',
              code: 'audience_missing',
              message: 'The audience preview was not included in the response.',
              retryable: true,
            })
        audienceErrorAfterInterpret.value = true
      }
    } catch (e) {
      const err = toApiError(e)
      if (err.isAbort || myEpoch !== epoch) return
      interpretError.value = err
    } finally {
      untrack(controller)
      if (myEpoch === epoch) interpreting.value = false
    }
  }

  /** GET /audience for the current campaign version. Aborts any previous refresh. */
  async function refreshAudience(): Promise<void> {
    const id = campaignId.value
    if (!id || !hasAudienceStep.value) return
    audienceController?.abort()
    const { controller, myEpoch } = track()
    audienceController = controller
    audienceLoading.value = true
    audienceError.value = null
    try {
      const res = await api.getAudience(id, controller.signal)
      if (myEpoch !== epoch || controller.signal.aborted) return
      const cur = campaign.value
      if (!cur || res.campaign_id !== cur.id) return
      if (res.campaign_version < cur.version) return // older than what is shown: drop
      if (res.campaign_version > cur.version) {
        staleNotice.value = true // changed elsewhere: never mix with local state
        return
      }
      setAudience(res.audience, res.campaign_version, res.channels)
    } catch (e) {
      const err = toApiError(e)
      if (err.isAbort || myEpoch !== epoch || controller.signal.aborted) return
      audienceError.value = err
      audienceErrorAfterInterpret.value = false
    } finally {
      untrack(controller)
      if (audienceController === controller) {
        audienceController = null
        audienceLoading.value = false
      }
    }
  }

  /**
   * PUT /channels with the current version. [] is a valid, explicit choice.
   * On success the response's version is adopted and the audience refreshed;
   * on 409 nothing is retried: the selection is kept for an explicit re-apply.
   */
  async function setChannels(next: Channel[]): Promise<void> {
    const id = campaignId.value
    const cur = campaign.value
    if (!id || !cur || busy.value || !canEditChannels.value || conflict.value) return
    const wanted = canonicalChannels(next)
    if (sameChannels(wanted, cur.channels)) return
    const { controller, myEpoch } = track()
    savingChannels.value = true
    pendingChannels.value = wanted
    channelsError.value = null
    try {
      const updated = await api.setChannels(id, wanted, cur.version, controller.signal)
      if (myEpoch !== epoch) return
      applyCampaign(updated)
      unsavedChannels.value = null
      pendingChannels.value = null
      savingChannels.value = false
      void refreshAudience()
      void refreshContent() // validation depends on the selected channels
    } catch (e) {
      const err = toApiError(e)
      if (err.isAbort || myEpoch !== epoch) return
      channelsError.value = err
      if (err.code === 'version_conflict') unsavedChannels.value = wanted
    } finally {
      untrack(controller)
      if (myEpoch === epoch) {
        savingChannels.value = false
        pendingChannels.value = null
      }
    }
  }

  /** Re-applies the selection rejected by a conflict, on top of the reloaded version. */
  async function reapplyUnsavedChannels(): Promise<void> {
    const wanted = unsavedChannels.value
    if (!wanted) return
    if (campaign.value && sameChannels(wanted, campaign.value.channels)) {
      unsavedChannels.value = null
      return
    }
    await setChannels(wanted)
  }

  function discardUnsavedChannels(): void {
    unsavedChannels.value = null
  }

  function dismissChannelsError(): void {
    if (!conflict.value) channelsError.value = null
  }

  // ---- content actions ----

  /** GET /content for the campaign version shown. Aborts any previous refresh. */
  async function refreshContent(): Promise<void> {
    const id = campaignId.value
    if (!id || !campaign.value?.segment_rule) return
    contentController?.abort()
    const { controller, myEpoch } = track()
    contentController = controller
    contentLoading.value = true
    contentError.value = null
    try {
      const view = await api.getContent(id, controller.signal)
      if (myEpoch !== epoch || controller.signal.aborted) return
      applyContent(view)
    } catch (e) {
      const err = toApiError(e)
      if (err.isAbort || myEpoch !== epoch || controller.signal.aborted) return
      contentError.value = err
    } finally {
      untrack(controller)
      if (contentController === controller) {
        contentController = null
        contentLoading.value = false
      }
    }
  }

  /** Updates one draft field (editor input). */
  function setDraftField(field: ContentField, value: string): void {
    if (draft.value[field] === value) return
    draft.value = { ...draft.value, [field]: value }
    draftRevision.value++
    saveNotice.value = null
    if (saveError.value?.code === 'validation_failed') saveError.value = null
  }

  /** Discards unsaved edits. */
  function discardDraft(): void {
    if (savingContent.value) return
    resetDraft()
    saveError.value = null
  }

  /**
   * POST /content/validate for the current draft revision (the editor calls
   * it debounced). Read-only; never blocks other actions. A clean draft is
   * not sent: the saved version's validation is shown instead.
   */
  async function validateDraft(): Promise<void> {
    const id = campaignId.value
    const cur = campaign.value
    validateController?.abort()
    validateController = null
    if (!id || !cur || !dirty.value) {
      validatingDraft.value = false
      return
    }
    const revision = draftRevision.value
    const version = cur.version
    const { controller, myEpoch } = track()
    validateController = controller
    validatingDraft.value = true
    draftValidationError.value = null
    try {
      const res = await api.validateContent(id, draftPayload(), controller.signal)
      if (myEpoch !== epoch || controller.signal.aborted) return
      if (res.campaign_id !== id || revision !== draftRevision.value) return
      if (res.campaign_version > (campaign.value?.version ?? 0)) {
        staleNotice.value = true
        return
      }
      if (res.campaign_version !== version || version !== campaign.value?.version) return
      draftValidation.value = res.validation
      draftValidationFor.value = { revision, version }
    } catch (e) {
      const err = toApiError(e)
      if (err.isAbort || myEpoch !== epoch || controller.signal.aborted) return
      draftValidationError.value = err
    } finally {
      untrack(controller)
      if (validateController === controller) {
        validateController = null
        validatingDraft.value = false
      }
    }
  }

  /**
   * PUT /content with the draft on top of the version shown. Unchanged
   * canonical text creates no version (saveNotice 'unchanged'). A 409 keeps
   * the draft for an explicit save after reloading; a 422 shows the
   * server's field errors.
   */
  async function saveContent(): Promise<void> {
    const id = campaignId.value
    const cur = campaign.value
    if (!id || !cur || busy.value || !canEditContent.value || !dirty.value) return
    // Only a draft whose current revision the server has validated (and whose
    // warnings are therefore shown) can be saved.
    if (!shownValidation.value?.valid) return
    const revision = draftRevision.value
    const { controller, myEpoch } = track()
    savingContent.value = true
    saveError.value = null
    saveNotice.value = null
    try {
      const saved = await api.saveContent(id, draftPayload(), cur.version, controller.signal)
      if (myEpoch !== epoch) return
      if (!applyCampaign(saved.campaign)) return
      // Edits typed while the request was in flight are kept.
      applyContent(saved.content, { resetDraft: revision === draftRevision.value })
      saveNotice.value = saved.created ? 'saved' : 'unchanged'
      if (saved.created) void refreshAudience()
    } catch (e) {
      const err = toApiError(e)
      if (err.isAbort || myEpoch !== epoch) return
      saveError.value = err
    } finally {
      untrack(controller)
      if (myEpoch === epoch) savingContent.value = false
    }
  }

  function onGenerationEvent(ev: GenerationEvent): void {
    const g = generation.value
    switch (ev.type) {
      case 'meta':
        generation.value = { ...g, fields: ev.data.fields }
        break
      case 'status': {
        const attempt = ev.data.attempt ?? g.attempt
        const next: GenerationView = { ...g, phase: ev.data.phase, attempt }
        if (ev.data.phase === 'generating') {
          next.reason = ev.data.reason ?? null
          next.problems = 0
          if (attempt !== g.attempt) next.preview = {} // a new attempt discards the previous draft
        }
        if (ev.data.phase === 'repairing') next.problems = ev.data.problems ?? 0
        generation.value = next
        break
      }
      case 'delta': {
        if (ev.data.attempt !== g.attempt) return
        const prev = g.preview[ev.data.field] ?? ''
        if (prev.length >= MAX_PREVIEW_CHARS) return
        generation.value = {
          ...g,
          preview: { ...g.preview, [ev.data.field]: (prev + ev.data.text).slice(0, MAX_PREVIEW_CHARS) },
        }
        break
      }
      default:
        break // terminal events are handled by the promise
    }
  }

  function applyGenerated(saved: ContentSaved): void {
    if (!applyCampaign(saved.campaign)) return
    applyContent(saved.content, { resetDraft: true })
    void refreshAudience()
  }

  /**
   * POST /content/generations (SSE). Refused while another mutation runs or
   * while the draft has unsaved edits (they would be replaced).
   */
  async function generateContent(): Promise<void> {
    const id = campaignId.value
    if (!id || busy.value || !canGenerate.value) return
    const { controller, myEpoch } = track()
    generationController = controller
    generationCancelled = false
    saveNotice.value = null
    saveError.value = null
    generation.value = { ...IDLE_GENERATION, phase: 'connecting' }
    try {
      const saved = await api.generateContent(
        id,
        (ev) => {
          // Stale events (another campaign, unmounted page, cancelled request) are dropped.
          if (myEpoch !== epoch || controller.signal.aborted) return
          onGenerationEvent(ev)
        },
        controller.signal,
      )
      if (myEpoch !== epoch) return
      applyGenerated(saved)
      generation.value = { ...generation.value, phase: 'done', preview: {}, notes: saved.notes, error: null }
    } catch (e) {
      const err = toApiError(e)
      if (myEpoch !== epoch) return
      if (err.isAbort) {
        generation.value = { ...generation.value, phase: generationCancelled ? 'cancelled' : 'idle', preview: {} }
      } else {
        // The preview of a failed generation is never kept.
        generation.value = { ...generation.value, phase: 'failed', preview: {}, error: err }
      }
      // A commit may have happened right before the failure or cancellation.
      void fetchCampaign({ refreshAudience: false })
    } finally {
      untrack(controller)
      if (generationController === controller) generationController = null
    }
  }

  /** Aborts the running generation (the server cancels the model call). */
  function cancelGeneration(): void {
    if (!generationController) return
    generationCancelled = true
    generationController.abort()
  }

  function dismissGeneration(): void {
    if (!generating.value) generation.value = IDLE_GENERATION
  }

  // ---- approval actions (CP15) ----

  function isLive(a: ApprovalSummary | null): boolean {
    return !!a && (a.status === 'pending' || a.status === 'approved')
  }

  /**
   * Applies an approval response: the campaign first (dropped if older than
   * the one shown), then the approval of exactly that campaign version.
   * Reports how a previously shown live approval ended.
   */
  function applyApproval(res: ApprovalResponse): boolean {
    const prev = approval.value
    const prevVersion = campaign.value?.version ?? 0
    if (!applyCampaign(res.campaign)) return false
    approval.value = res.approval
    approvalError.value = null
    const offset = Date.parse(res.server_time) - Date.now()
    if (Number.isFinite(offset)) serverOffsetMs.value = offset
    const a = res.approval
    if (isLive(prev) && prev!.approval_id === a.approval_id && !isLive(a)) {
      approvalDialogOpen.value = false
      if (a.status === 'expired') approvalNotice.value = 'expired'
      else if (a.revoke_reason === 'withdrawn') approvalNotice.value = 'withdrawn'
      else approvalNotice.value = 'replaced'
    }
    if (res.campaign.version !== prevVersion) {
      // Content validation and audience are bound to the campaign version.
      if (res.campaign.segment_rule) void refreshContent()
      if (AUDIENCE_STATUSES.includes(res.campaign.status)) void refreshAudience()
    }
    return true
  }

  /**
   * Keeps the shown approval in step with a freshly loaded campaign: loads
   * its live approval, or — if the approval shown is no longer live (it
   * expired, or another tab withdrew it) — re-reads it to show how it ended.
   */
  function syncApproval(c: Campaign, opts: { restoreDialog: boolean }): void {
    const shown = approval.value
    if (c.active_approval_id && (c.status === 'pending_approval' || c.status === 'approved')) {
      if (!shown || shown.approval_id !== c.active_approval_id || shown.campaign_version !== c.version) {
        void refreshApproval(c.active_approval_id, { openDialog: opts.restoreDialog && c.status === 'pending_approval' })
      }
      return
    }
    if (shown && isLive(shown) && shown.campaign_id === c.id) void refreshApproval(shown.approval_id)
  }

  /** GET …/approvals/{aid}. The server applies lazy expiry first. */
  async function refreshApproval(approvalId?: string, opts: { openDialog?: boolean } = {}): Promise<void> {
    const id = campaignId.value
    const aid = approvalId ?? approval.value?.approval_id ?? campaign.value?.active_approval_id ?? null
    if (!id || !aid) return
    approvalController?.abort()
    const { controller, myEpoch } = track()
    approvalController = controller
    approvalLoading.value = true
    approvalError.value = null
    try {
      const res = await api.getApproval(id, aid, controller.signal)
      if (myEpoch !== epoch || controller.signal.aborted) return
      if (!applyApproval(res)) return
      if (opts.openDialog && res.approval.status === 'pending') approvalDialogOpen.value = true
    } catch (e) {
      const err = toApiError(e)
      if (err.isAbort || myEpoch !== epoch || controller.signal.aborted) return
      approvalError.value = err
    } finally {
      untrack(controller)
      if (approvalController === controller) {
        approvalController = null
        approvalLoading.value = false
      }
    }
  }

  /**
   * POST /approvals on top of the version shown: freezes the audience into a
   * pending approval and opens the review dialog. Refused while anything
   * else is pending or the content is not saved and valid.
   */
  async function prepareApproval(): Promise<void> {
    const id = campaignId.value
    const cur = campaign.value
    if (!id || !cur || !canPrepare.value) return
    approvalController?.abort()
    const { controller, myEpoch } = track()
    preparing.value = true
    approvalActionError.value = null
    approvalNotice.value = null
    try {
      const res = await api.prepareApproval(id, cur.version, controller.signal)
      if (myEpoch !== epoch) return
      if (!applyApproval(res)) return
      approvalDialogOpen.value = true
    } catch (e) {
      const err = toApiError(e)
      if (err.isAbort || myEpoch !== epoch) return
      approvalActionError.value = { op: 'prepareApproval', error: err }
    } finally {
      untrack(controller)
      if (myEpoch === epoch) preparing.value = false
    }
  }

  /**
   * POST …/confirm with exactly the binding shown (approval id, campaign
   * version, segment version, content version, channels). Allowed only for
   * the live pending approval of the version shown.
   */
  async function confirmApproval(): Promise<void> {
    const id = campaignId.value
    const cur = campaign.value
    const a = liveApproval.value
    if (!id || !cur || !a || a.status !== 'pending' || busy.value) return
    const { controller, myEpoch } = track()
    confirming.value = true
    approvalActionError.value = null
    try {
      const res = await api.confirmApproval(
        id,
        a.approval_id,
        {
          expected_version: cur.version,
          segment_version: a.segment_version,
          content_version: a.content_version,
          channels: a.channels,
        },
        controller.signal,
      )
      if (myEpoch !== epoch) return
      if (!applyApproval(res)) return
      approvalDialogOpen.value = false
      approvalNotice.value = 'confirmed'
    } catch (e) {
      const err = toApiError(e)
      if (err.isAbort || myEpoch !== epoch) return
      approvalActionError.value = { op: 'confirmApproval', error: err }
      // An expiry is final and already committed: show it (a read, not a retry).
      if (err.code === 'approval_expired') void refreshApproval(a.approval_id)
    } finally {
      untrack(controller)
      if (myEpoch === epoch) confirming.value = false
    }
  }

  /** POST …/withdraw for the live approval shown (pending or approved). */
  async function withdrawApproval(): Promise<void> {
    const id = campaignId.value
    const cur = campaign.value
    const a = liveApproval.value
    if (!id || !cur || !a || busy.value) return
    const { controller, myEpoch } = track()
    withdrawing.value = true
    approvalActionError.value = null
    try {
      const res = await api.withdrawApproval(id, a.approval_id, cur.version, controller.signal)
      if (myEpoch !== epoch) return
      applyApproval(res)
    } catch (e) {
      const err = toApiError(e)
      if (err.isAbort || myEpoch !== epoch) return
      approvalActionError.value = { op: 'withdrawApproval', error: err }
      if (err.code === 'approval_expired') void refreshApproval(a.approval_id)
    } finally {
      untrack(controller)
      if (myEpoch === epoch) withdrawing.value = false
    }
  }

  function openApprovalDialog(): void {
    if (liveApproval.value?.status === 'pending') approvalDialogOpen.value = true
  }

  /** Closes the dialog without deciding: the approval stays pending until it expires. */
  function closeApprovalDialog(): void {
    approvalDialogOpen.value = false
  }

  /** The local countdown reached expires_at: let the server decide. */
  function approvalDeadlinePassed(): void {
    const a = liveApproval.value
    if (!a || approvalLoading.value || approvalBusy.value) return
    void refreshApproval(a.approval_id)
  }

  function dismissApprovalNotice(): void {
    approvalNotice.value = null
  }

  function dismissApprovalError(): void {
    approvalActionError.value = null
  }

  // ---- send / cancel (CP17B) ----

  /**
   * Re-reads the campaign after an interrupted or refused delivery action
   * (never guesses the outcome). Also used when the progress stream reports
   * a newer campaign version.
   */
  async function syncCampaign(): Promise<void> {
    if (!campaignId.value || loading.value) return
    await fetchCampaign({ refreshAudience: false })
  }

  /** Called by the dashboard with the campaign version a stats snapshot reports. */
  function noteServerVersion(version: number): void {
    const c = campaign.value
    if (!c || version <= c.version || busy.value) return
    void syncCampaign()
  }

  /**
   * POST /send for the confirmed approval of the version shown. Explicit
   * user action only; refused while anything else is pending.
   */
  async function startSending(): Promise<void> {
    const id = campaignId.value
    const cur = campaign.value
    const a = liveApproval.value
    if (!id || !cur || !a || !canSend.value) return
    const { controller, myEpoch } = track()
    sending.value = true
    deliveryActionError.value = null
    deliveryNotice.value = null
    try {
      const res = await api.send(id, { approval_id: a.approval_id, expected_version: cur.version }, controller.signal)
      if (myEpoch !== epoch) return
      if (res.campaign.id !== id) return
      applyCampaign(res.campaign)
      lastSend.value = res
      approvalNotice.value = null
      const released = Object.values(res.released).reduce((n, x) => n + (x ?? 0), 0)
      deliveryNotice.value = res.already_started ? { kind: 'already_started' } : { kind: 'started', released }
    } catch (e) {
      const err = toApiError(e)
      if (err.isAbort || myEpoch !== epoch) return
      deliveryActionError.value = { op: 'send', error: err }
      if (err.code === 'approval_expired') void refreshApproval(a.approval_id)
      // The outcome is unknown (lost response) or the state moved: re-read, never retry.
      if (err.kind === 'network' || err.kind === 'invalid_response' || err.code === 'invalid_state') {
        void reconcileAfter('send', myEpoch)
      }
    } finally {
      untrack(controller)
      if (myEpoch === epoch) sending.value = false
    }
  }

  async function reconcileAfter(op: DeliveryAction, myEpoch: number): Promise<void> {
    const before = campaign.value?.version ?? 0
    await fetchCampaign({ refreshAudience: false })
    if (myEpoch !== epoch) return
    const now = campaign.value
    if (now && now.version > before) deliveryNotice.value = { kind: 'reconciled', op }
  }

  /** POST /cancel for a sending campaign (stops new work; cannot recall requests in flight). */
  async function cancelSending(): Promise<void> {
    const id = campaignId.value
    const cur = campaign.value
    if (!id || !cur || !canCancelSending.value) return
    const { controller, myEpoch } = track()
    cancelling.value = true
    deliveryActionError.value = null
    deliveryNotice.value = null
    try {
      const res = await api.cancel(id, cur.version, controller.signal)
      if (myEpoch !== epoch) return
      if (res.campaign.id !== id) return
      applyCampaign(res.campaign)
      lastCancel.value = res
      const sum = (m: Partial<Record<Channel, number>>) => Object.values(m).reduce((n, x) => n + (x ?? 0), 0)
      deliveryNotice.value = {
        kind: 'cancelled',
        cancelled: sum(res.cancelled_deliveries),
        inFlight: sum(res.in_flight),
        releasedHeld: res.released_held_rows,
      }
    } catch (e) {
      const err = toApiError(e)
      if (err.isAbort || myEpoch !== epoch) return
      deliveryActionError.value = { op: 'cancel', error: err }
      // A lost response may have cancelled; an invalid_state usually means the
      // campaign just completed (cancellation racing completion): re-read.
      if (err.kind === 'network' || err.kind === 'invalid_response' || err.code === 'invalid_state') {
        void reconcileAfter('cancel', myEpoch)
      }
    } finally {
      untrack(controller)
      if (myEpoch === epoch) cancelling.value = false
    }
  }

  function dismissDeliveryNotice(): void {
    deliveryNotice.value = null
  }

  function dismissDeliveryError(): void {
    deliveryActionError.value = null
  }

  return {
    // state
    campaignId,
    campaign,
    loading,
    loadError,
    interpreting,
    interpretError,
    audience,
    audienceForVersion,
    audienceLoading,
    audienceError,
    audienceErrorAfterInterpret,
    savingChannels,
    pendingChannels,
    channelsError,
    unsavedChannels,
    staleNotice,
    content,
    contentLoading,
    contentError,
    draft,
    draftRevision,
    draftBaseId,
    validatingDraft,
    draftValidationError,
    savingContent,
    saveError,
    saveNotice,
    generation,
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
    sending,
    cancelling,
    deliveryActionError,
    deliveryNotice,
    lastCancel,
    lastSend,
    // derived
    busy,
    approvalBusy,
    approvalCurrent,
    liveApproval,
    prepareBlockedReason,
    canPrepare,
    status,
    canInterpret,
    canEditChannels,
    hasAudienceStep,
    selectedChannels,
    conflict,
    audienceCurrent,
    generating,
    contentCurrent,
    editableFields,
    dirty,
    hasContentStep,
    canEditContent,
    canGenerate,
    shownValidation,
    deliveryBusy,
    hasDeliveryStep,
    sendBlockedReason,
    canSend,
    canCancelSending,
    // actions
    open,
    close,
    adopt,
    reload,
    interpret,
    refreshAudience,
    setChannels,
    reapplyUnsavedChannels,
    discardUnsavedChannels,
    dismissChannelsError,
    refreshContent,
    setDraftField,
    discardDraft,
    validateDraft,
    saveContent,
    generateContent,
    cancelGeneration,
    dismissGeneration,
    refreshApproval,
    prepareApproval,
    confirmApproval,
    withdrawApproval,
    openApprovalDialog,
    closeApprovalDialog,
    approvalDeadlinePassed,
    dismissApprovalNotice,
    dismissApprovalError,
    syncCampaign,
    noteServerVersion,
    startSending,
    cancelSending,
    dismissDeliveryNotice,
    dismissDeliveryError,
  }
})
