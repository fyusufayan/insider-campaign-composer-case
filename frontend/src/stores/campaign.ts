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
//   4. One mutation at a time: interpret and channel updates both bump the
//      campaign version, so while one is pending the other is refused and
//      the UI disables its controls. Stale writes are never retried
//      automatically: a 409 keeps the user's selection aside for an explicit
//      re-apply after reloading.

import { defineStore } from 'pinia'
import { computed, ref, shallowRef } from 'vue'
import { ApiError, api, isApiError } from '@/api/client'
import type { AudienceStats, Campaign, CampaignStatus, Channel } from '@/api/types'
import { CHANNELS } from '@/api/types'

const AUDIENCE_STATUSES: readonly CampaignStatus[] = ['segmented', 'content_ready', 'pending_approval', 'approved']
const INTERPRET_STATUSES: readonly CampaignStatus[] = ['draft', 'segmented', 'content_ready']
const CHANNEL_STATUSES: readonly CampaignStatus[] = ['segmented', 'content_ready']

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

  let epoch = 0
  const controllers = new Set<AbortController>()
  let audienceController: AbortController | null = null

  // ---- derived ----
  const busy = computed(() => interpreting.value || savingChannels.value)
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
    await fetchCampaign({ refreshAudience: true })
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

  async function fetchCampaign(opts: { refreshAudience: boolean }): Promise<void> {
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
    // derived
    busy,
    status,
    canInterpret,
    canEditChannels,
    hasAudienceStep,
    selectedChannels,
    conflict,
    audienceCurrent,
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
  }
})
