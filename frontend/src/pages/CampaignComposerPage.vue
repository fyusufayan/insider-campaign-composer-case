<script setup lang="ts">
// Composer for one campaign, addressed by a stable URL (/campaigns/:id).
// Refresh and direct navigation load the campaign from the API; leaving the
// page (or switching to another id) aborts pending requests and drops their
// results (store epoch).
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import { storeToRefs } from 'pinia'
import { useCampaignStore } from '@/stores/campaign'
import { isCampaignId, useRecentCampaigns } from '@/composables/useRecentCampaigns'
import { formatDateTime } from '@/composables/format'
import type { Channel } from '@/api/types'
import ApprovalPanel from '@/components/composer/ApprovalPanel.vue'
import AudienceCard from '@/components/composer/AudienceCard.vue'
import BriefCard from '@/components/composer/BriefCard.vue'
import ContentPanel from '@/components/composer/ContentPanel.vue'
import InterpretationCard from '@/components/composer/InterpretationCard.vue'
import SegmentRuleView from '@/components/composer/SegmentRuleView.vue'
import ErrorNotice from '@/components/ui/ErrorNotice.vue'
import StatusBadge from '@/components/ui/StatusBadge.vue'

const props = defineProps<{ id: string }>()

const router = useRouter()
const store = useCampaignStore()
const recent = useRecentCampaigns()
const {
  campaign,
  loading,
  loadError,
  interpreting,
  interpretError,
  audience,
  audienceCurrent,
  audienceLoading,
  audienceError,
  audienceErrorAfterInterpret,
  savingChannels,
  channelsError,
  unsavedChannels,
  staleNotice,
  busy,
  canInterpret,
  canEditChannels,
  hasAudienceStep,
  hasContentStep,
  selectedChannels,
  conflict,
} = storeToRefs(store)

const invalidId = computed(() => !isCampaignId(props.id))

watch(
  () => props.id,
  (id) => {
    if (!isCampaignId(id)) {
      store.close()
      return
    }
    // A campaign just created was adopted by the store: no second GET.
    if (store.campaignId === id && store.campaign) {
      if (store.hasAudienceStep && !store.audience) void store.refreshAudience()
      return
    }
    void store.open(id)
  },
  { immediate: true },
)

// flush 'post': runs after the router's afterEach sets the generic title,
// including for a campaign adopted right after creation.
watch(
  campaign,
  (c) => {
    if (c) {
      recent.remember(c.id, c.name)
      document.title = `${c.name} · Campaign Composer`
    }
  },
  { immediate: true, flush: 'post' },
)

onBeforeUnmount(() => store.close())

function onChangeChannels(chs: Channel[]) {
  void store.setChannels(chs)
}

function revise() {
  const c = store.campaign
  if (!c) return
  void router.push({ name: 'campaign-new', state: { prefill: { name: c.name, brief: c.brief } } })
}

const copied = ref(false)
async function copyLink() {
  try {
    await navigator.clipboard.writeText(window.location.href)
    copied.value = true
    setTimeout(() => (copied.value = false), 1500)
  } catch {
    copied.value = false
  }
}
</script>

<template>
  <div class="page">
    <nav class="crumbs" aria-label="Breadcrumb">
      <RouterLink :to="{ name: 'campaign-new' }">Campaigns</RouterLink>
      <span aria-hidden="true">/</span>
      <span aria-current="page">{{ campaign?.name ?? 'Campaign' }}</span>
    </nav>

    <!-- Invalid id in the URL -->
    <div v-if="invalidId" class="card empty-card" data-testid="invalid-id">
      <h1>Invalid campaign link</h1>
      <p>This URL does not contain a valid campaign ID.</p>
      <RouterLink :to="{ name: 'campaign-new' }" class="btn btn-primary">Create a campaign</RouterLink>
    </div>

    <!-- Initial load -->
    <div v-else-if="!campaign && loading" class="card empty-card" role="status" data-testid="campaign-loading">
      <span class="spinner" aria-hidden="true" /> Loading campaign…
    </div>

    <!-- Load failed -->
    <div v-else-if="!campaign && loadError" class="load-error" data-testid="load-error">
      <ErrorNotice :error="loadError" op="load" retry-label="Try again" @retry="store.open(id)" />
      <RouterLink :to="{ name: 'campaign-new' }" class="btn btn-secondary back">Create a new campaign</RouterLink>
    </div>

    <template v-else-if="campaign">
      <header class="page-header">
        <div class="title-row">
          <h1 data-testid="campaign-name">{{ campaign.name }}</h1>
          <StatusBadge :status="campaign.status" />
        </div>
        <p class="meta">
          <span>
            ID <code class="mono" data-testid="campaign-id">{{ campaign.id }}</code>
            <button type="button" class="btn btn-ghost btn-sm copy" @click="copyLink">
              {{ copied ? 'Link copied' : 'Copy link' }}
            </button>
          </span>
          <span>Version {{ campaign.version }}</span>
          <span>Updated {{ formatDateTime(campaign.updated_at) }}</span>
        </p>
      </header>

      <div v-if="staleNotice" class="notice notice-warning stale" role="alert" data-testid="stale-notice">
        <div>
          <p class="notice-title">A newer version of this campaign exists</p>
          <p>It was changed in another tab or session. Reload to see the latest state.</p>
          <div class="notice-actions">
            <button type="button" class="btn btn-secondary btn-sm" :disabled="busy" @click="store.reload()">
              Reload latest campaign
            </button>
          </div>
        </div>
      </div>
      <ErrorNotice
        v-if="loadError"
        class="stale"
        :error="loadError"
        op="load"
        tone="warning"
        @retry="store.reload()"
      />

      <div class="grid">
        <div class="col">
          <BriefCard
            :campaign="campaign"
            :interpreting="interpreting"
            :can-interpret="canInterpret"
            :busy="busy"
            :error="interpretError"
            @interpret="store.interpret()"
            @reload="store.reload()"
            @revise="revise"
          />
          <InterpretationCard
            v-if="campaign.interpretation"
            :interpretation="campaign.interpretation"
            :content-brief="campaign.content_brief"
          />
          <section v-else class="card placeholder" data-testid="interpretation-placeholder">
            <div class="card-body">
              <p class="card-eyebrow"><span class="step-num">2</span> What the AI understood</p>
              <p class="placeholder-text">
                {{ interpreting ? 'Waiting for the interpretation…' : 'Not interpreted yet. Run “Interpret brief” to get a proposal.' }}
              </p>
            </div>
          </section>
        </div>

        <div class="col">
          <template v-if="campaign.segment_rule">
            <SegmentRuleView :rule="campaign.segment_rule" :segment-version="campaign.segment_version" />
            <AudienceCard
              v-if="hasAudienceStep"
              :stats="audience"
              :current="audienceCurrent"
              :loading="audienceLoading"
              :error="audienceError"
              :error-after-interpret="audienceErrorAfterInterpret"
              :selected-channels="selectedChannels"
              :saved-channels="campaign.channels"
              :can-edit-channels="canEditChannels"
              :busy="busy"
              :saving-channels="savingChannels"
              :channels-error="channelsError"
              :conflict="conflict"
              :unsaved-channels="unsavedChannels"
              @change-channels="onChangeChannels"
              @retry-audience="store.refreshAudience()"
              @reload="store.reload()"
              @reapply="store.reapplyUnsavedChannels()"
              @discard="store.discardUnsavedChannels()"
              @dismiss-channels-error="store.dismissChannelsError()"
            />
          </template>
          <section v-else class="card placeholder" data-testid="audience-placeholder">
            <div class="card-body">
              <p class="card-eyebrow"><span class="step-num">3</span> Active targeting &amp; audience</p>
              <p class="placeholder-text">
                After interpretation you will see the exact targeting conditions, live audience counts per channel and
                the channel selection here.
              </p>
            </div>
          </section>
        </div>
      </div>

      <ContentPanel v-if="hasContentStep" class="content-step" @revise="revise" />
      <ApprovalPanel v-if="hasContentStep" class="content-step" />
    </template>
  </div>
</template>

<style scoped>
.crumbs {
  display: flex;
  gap: var(--space-2);
  font-size: 13px;
  color: var(--c-muted);
  margin-bottom: var(--space-3);
  min-width: 0;
}
.crumbs a {
  color: var(--c-muted);
}
.crumbs span[aria-current] {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.page-header {
  margin-bottom: var(--space-5);
}
.title-row {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: var(--space-3);
}
.title-row h1 {
  font-size: 26px;
  font-weight: 750;
  overflow-wrap: anywhere;
}
.meta {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--space-2) var(--space-5);
  margin-top: var(--space-2);
  color: var(--c-muted);
  font-size: 13px;
}
.meta code {
  color: var(--c-text-2);
}
.copy {
  margin-left: 4px;
}
.stale {
  margin-bottom: var(--space-4);
}
.grid {
  display: grid;
  grid-template-columns: minmax(0, 5fr) minmax(0, 7fr);
  gap: var(--space-5);
  align-items: start;
}
.col {
  display: flex;
  flex-direction: column;
  gap: var(--space-5);
  min-width: 0;
}
.content-step {
  margin-top: var(--space-5);
}
.placeholder {
  border-style: dashed;
  box-shadow: none;
  background: transparent;
}
.placeholder .card-body {
  padding-top: var(--space-5);
}
.placeholder-text {
  color: var(--c-muted);
  font-size: 14px;
  margin-top: var(--space-2);
}
.empty-card {
  padding: var(--space-8);
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: var(--space-3);
}
.empty-card[role='status'] {
  flex-direction: row;
  align-items: center;
}
.load-error {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: var(--space-4);
}
@media (max-width: 1024px) {
  .grid {
    grid-template-columns: 1fr;
  }
}
</style>
