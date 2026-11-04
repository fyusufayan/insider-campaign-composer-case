<script setup lang="ts">
// "How many users match / how many messages are eligible": the backend's
// audience partition (ARCHITECTURE §6.5), shown as returned. Users and
// messages are labelled separately and never added together.
import { computed } from 'vue'
import type { ApiError } from '@/api/client'
import type { AudienceStats, Channel, ChannelStats } from '@/api/types'
import { CHANNEL_LABEL, formatCount, formatDateTime, percent } from '@/composables/format'
import ErrorNotice from '@/components/ui/ErrorNotice.vue'
import ChannelSelector from './ChannelSelector.vue'

const props = defineProps<{
  stats: AudienceStats | null
  /** stats belong to the campaign version and channels shown. */
  current: boolean
  loading: boolean
  error: ApiError | null
  errorAfterInterpret: boolean
  /** Channels shown in the selector (pending selection while saving). */
  selectedChannels: Channel[]
  /** Saved channels of the campaign. */
  savedChannels: Channel[]
  canEditChannels: boolean
  busy: boolean
  savingChannels: boolean
  channelsError: ApiError | null
  conflict: boolean
  unsavedChannels: Channel[] | null
}>()

const emit = defineEmits<{
  'change-channels': [channels: Channel[]]
  'retry-audience': []
  reload: []
  reapply: []
  discard: []
  'dismiss-channels-error': []
}>()

const CHANNEL_ORDER: Channel[] = ['sms', 'email']

/** Channels of the displayed statistics (keys of by_channel). */
const statChannels = computed<Channel[]>(() =>
  props.stats ? CHANNEL_ORDER.filter((c) => props.stats?.by_channel[c] !== undefined) : [],
)

function channelStats(ch: Channel): ChannelStats | undefined {
  return props.stats?.by_channel[ch]
}

function eligibleMessages(ch: Channel): number {
  return channelStats(ch)?.eligible ?? 0
}

function channelSum(s: ChannelStats): number {
  return s.eligible + s.no_consent + s.no_contact
}

const reachSum = computed(() => {
  const r = props.stats?.reach_users
  return r ? r.both + r.sms_only + r.email_only + r.unreachable : 0
})

const identitiesOk = computed(() => {
  const s = props.stats
  if (!s) return true
  const chOk = statChannels.value.every((c) => {
    const cs = s.by_channel[c]
    return cs !== undefined && channelSum(cs) === s.matched_users
  })
  const msgOk = statChannels.value.reduce((n, c) => n + (s.by_channel[c]?.eligible ?? 0), 0) === s.messages
  return chOk && msgOk && reachSum.value === s.matched_users
})

const noChannels = computed(() => !!props.stats && statChannels.value.length === 0)
const showStale = computed(() => !!props.stats && (!props.current || props.loading))

const unsavedLabel = computed(() =>
  props.unsavedChannels
    ? props.unsavedChannels.length
      ? props.unsavedChannels.map((c) => CHANNEL_LABEL[c]).join(' + ')
      : 'no channels'
    : '',
)

function width(part: number, whole: number): string {
  return `${percent(part, whole).toFixed(2)}%`
}
</script>

<template>
  <section class="card" aria-labelledby="audience-heading" data-testid="audience-card">
    <div class="card-header">
      <div>
        <p class="card-eyebrow"><span class="step-num">4</span> Who you reach</p>
        <h2 id="audience-heading" class="card-title">Audience &amp; channels</h2>
        <p class="card-subtitle">
          Live, informational preview from the customer database. Nothing is frozen or sent from this screen.
        </p>
      </div>
      <button
        v-if="stats || error"
        type="button"
        class="btn btn-ghost btn-sm"
        :disabled="loading || busy"
        data-testid="refresh-audience"
        @click="emit('retry-audience')"
      >
        Refresh
      </button>
    </div>

    <div class="card-body">
      <ChannelSelector
        :selected="selectedChannels"
        :disabled="!canEditChannels || busy || conflict"
        :saving="savingChannels"
        @change="emit('change-channels', $event)"
      />
      <p v-if="!canEditChannels" class="field-help locked">Channels cannot be changed in this campaign status.</p>

      <!-- Channel update errors -->
      <div v-if="conflict" class="notice notice-warning block" role="alert" data-testid="channels-conflict">
        <div>
          <p class="notice-title">This campaign was changed elsewhere</p>
          <p>
            Your selection (<strong>{{ unsavedLabel }}</strong>) was <strong>not saved</strong> because the campaign
            was modified by another request. Reload the latest version; you can then re-apply your selection.
          </p>
          <div class="notice-actions">
            <button type="button" class="btn btn-secondary btn-sm" :disabled="busy" data-testid="conflict-reload" @click="emit('reload')">
              Reload latest campaign
            </button>
          </div>
        </div>
      </div>
      <div
        v-else-if="unsavedChannels"
        class="notice notice-info block"
        role="status"
        data-testid="unsaved-channels"
      >
        <div>
          <p class="notice-title">Latest campaign loaded</p>
          <p>
            Saved channels: <strong>{{ savedChannels.length ? savedChannels.map((c) => CHANNEL_LABEL[c]).join(' + ') : 'none' }}</strong>.
            Your unsaved selection was <strong>{{ unsavedLabel }}</strong>.
          </p>
          <div class="notice-actions">
            <button type="button" class="btn btn-primary btn-sm" :disabled="busy || !canEditChannels" data-testid="reapply-channels" @click="emit('reapply')">
              Re-apply my selection
            </button>
            <button type="button" class="btn btn-ghost btn-sm" @click="emit('discard')">Keep saved channels</button>
          </div>
        </div>
      </div>
      <ErrorNotice
        v-else-if="channelsError"
        class="block"
        :error="channelsError"
        op="channels"
        retry-label="Dismiss"
        @reload="emit('reload')"
        @retry="emit('dismiss-channels-error')"
      />

      <!-- Audience preview errors -->
      <div
        v-if="error && errorAfterInterpret"
        class="notice notice-warning block"
        role="alert"
        data-testid="audience-after-interpret-error"
      >
        <div>
          <p class="notice-title">Interpretation saved — audience preview unavailable</p>
          <p>
            The interpretation and targeting rule <strong>were saved</strong>. Only the audience statistics could not be
            computed: {{ error.message }}
          </p>
          <div class="notice-actions">
            <button
              type="button"
              class="btn btn-secondary btn-sm"
              :disabled="loading"
              data-testid="retry-audience"
              @click="emit('retry-audience')"
            >
              <span v-if="loading" class="spinner" aria-hidden="true" />
              Retry audience preview
            </button>
          </div>
        </div>
      </div>
      <ErrorNotice
        v-else-if="error"
        class="block"
        :error="error"
        op="audience"
        :busy="loading"
        retry-label="Retry audience preview"
        @retry="emit('retry-audience')"
        @reload="emit('reload')"
      />

      <!-- Loading without data -->
      <div v-if="!stats && loading" class="skeleton" role="status" aria-live="polite" data-testid="audience-loading">
        <span class="spinner" aria-hidden="true" /> Computing audience…
      </div>

      <!-- Statistics -->
      <div v-if="stats" class="stats" :class="{ stale: showStale }" data-testid="audience-stats" :aria-busy="loading ? 'true' : 'false'">
        <p v-if="showStale" class="stale-label" role="status" data-testid="audience-stale">
          <span v-if="loading" class="spinner" aria-hidden="true" />
          {{ loading ? 'Updating for the current selection…' : 'These numbers are from a previous selection.' }}
        </p>

        <div class="kpis">
          <div class="kpi kpi-users" data-testid="kpi-matched">
            <span class="kpi-label">Matched users</span>
            <span class="kpi-value num">{{ formatCount(stats.matched_users) }}</span>
            <span class="kpi-unit">people matching the rule</span>
          </div>
          <div v-for="ch in CHANNEL_ORDER" :key="ch" class="kpi" :data-testid="`kpi-${ch}`">
            <span class="kpi-label">Eligible {{ CHANNEL_LABEL[ch] }} messages</span>
            <span class="kpi-value num" :class="{ muted: !statChannels.includes(ch) }">
              {{ formatCount(eligibleMessages(ch)) }}
            </span>
            <span class="kpi-unit">{{ statChannels.includes(ch) ? 'messages' : 'channel not selected' }}</span>
          </div>
          <div class="kpi kpi-total" data-testid="kpi-total">
            <span class="kpi-label">Total eligible messages</span>
            <span class="kpi-value num">{{ formatCount(stats.messages) }}</span>
            <span class="kpi-unit">messages across selected channels</span>
          </div>
        </div>

        <div v-if="noChannels" class="notice notice-info block" role="note" data-testid="no-channels">
          <div>
            <p class="notice-title">No delivery channels selected</p>
            <p>
              No messages would be sent (0 messages). The rule still matches
              <strong class="num">{{ formatCount(stats.matched_users) }}</strong> users — select SMS or email to see who
              is reachable.
            </p>
          </div>
        </div>

        <p v-if="stats.matched_users === 0" class="notice notice-info block" role="note" data-testid="no-match">
          No users match this rule right now.
        </p>

        <!-- Per-channel partitions -->
        <div v-for="ch in statChannels" :key="ch" class="partition" :data-testid="`partition-${ch}`">
          <div class="partition-head">
            <h3 class="partition-title">{{ CHANNEL_LABEL[ch] }} · matched users by eligibility</h3>
          </div>
          <div
            class="bar"
            role="img"
            :aria-label="`${CHANNEL_LABEL[ch]}: ${channelStats(ch)?.eligible} eligible, ${channelStats(ch)?.no_consent} no consent, ${channelStats(ch)?.no_contact} no contact`"
          >
            <span class="seg eligible" :style="{ width: width(channelStats(ch)?.eligible ?? 0, stats.matched_users) }" />
            <span class="seg no-consent" :style="{ width: width(channelStats(ch)?.no_consent ?? 0, stats.matched_users) }" />
            <span class="seg no-contact" :style="{ width: width(channelStats(ch)?.no_contact ?? 0, stats.matched_users) }" />
          </div>
          <dl class="legend">
            <div>
              <dt><span class="dot" style="background: var(--c-eligible)" />Eligible</dt>
              <dd class="num" :data-testid="`${ch}-eligible`">{{ formatCount(channelStats(ch)?.eligible ?? 0) }}</dd>
            </div>
            <div>
              <dt><span class="dot" style="background: var(--c-no-consent)" />No consent</dt>
              <dd class="num" :data-testid="`${ch}-no_consent`">{{ formatCount(channelStats(ch)?.no_consent ?? 0) }}</dd>
            </div>
            <div>
              <dt><span class="dot" style="background: var(--c-no-contact)" />No contact</dt>
              <dd class="num" :data-testid="`${ch}-no_contact`">{{ formatCount(channelStats(ch)?.no_contact ?? 0) }}</dd>
            </div>
          </dl>
          <p class="identity num" :data-testid="`${ch}-identity`">
            {{ formatCount(channelStats(ch)?.eligible ?? 0) }} eligible + {{ formatCount(channelStats(ch)?.no_consent ?? 0) }} no
            consent + {{ formatCount(channelStats(ch)?.no_contact ?? 0) }} no contact =
            <strong>{{ formatCount(stats.matched_users) }} matched users</strong>
          </p>
        </div>

        <!-- Reach partition -->
        <div class="partition" data-testid="partition-reach">
          <div class="partition-head">
            <h3 class="partition-title">Reach · matched users by reachable channels</h3>
          </div>
          <div
            class="bar"
            role="img"
            :aria-label="`Reach: ${stats.reach_users.both} both, ${stats.reach_users.sms_only} SMS only, ${stats.reach_users.email_only} email only, ${stats.reach_users.unreachable} unreachable`"
          >
            <span class="seg both" :style="{ width: width(stats.reach_users.both, stats.matched_users) }" />
            <span class="seg sms" :style="{ width: width(stats.reach_users.sms_only, stats.matched_users) }" />
            <span class="seg email" :style="{ width: width(stats.reach_users.email_only, stats.matched_users) }" />
            <span class="seg unreachable" :style="{ width: width(stats.reach_users.unreachable, stats.matched_users) }" />
          </div>
          <dl class="legend four">
            <div>
              <dt><span class="dot" style="background: var(--c-both)" />Both</dt>
              <dd class="num" data-testid="reach-both">{{ formatCount(stats.reach_users.both) }}</dd>
            </div>
            <div>
              <dt><span class="dot" style="background: var(--c-sms)" />SMS only</dt>
              <dd class="num" data-testid="reach-sms_only">{{ formatCount(stats.reach_users.sms_only) }}</dd>
            </div>
            <div>
              <dt><span class="dot" style="background: var(--c-email)" />Email only</dt>
              <dd class="num" data-testid="reach-email_only">{{ formatCount(stats.reach_users.email_only) }}</dd>
            </div>
            <div>
              <dt><span class="dot" style="background: var(--c-unreachable)" />Unreachable</dt>
              <dd class="num" data-testid="reach-unreachable">{{ formatCount(stats.reach_users.unreachable) }}</dd>
            </div>
          </dl>
          <p class="identity num" data-testid="reach-identity">
            {{ formatCount(stats.reach_users.both) }} both + {{ formatCount(stats.reach_users.sms_only) }} SMS only +
            {{ formatCount(stats.reach_users.email_only) }} email only + {{ formatCount(stats.reach_users.unreachable) }}
            unreachable = <strong>{{ formatCount(stats.matched_users) }} matched users</strong>
          </p>
          <p class="field-help">“Unreachable” = matched users not eligible on any selected channel.</p>
        </div>

        <p v-if="!identitiesOk" class="notice notice-danger block" role="alert" data-testid="identity-violation">
          These statistics do not add up. Refresh the preview; if it persists, report it.
        </p>

        <p class="evaluated">
          Evaluated
          <time :datetime="stats.computed_at" data-testid="computed-at">{{ formatDateTime(stats.computed_at) }}</time>
          · counts are users unless labelled as messages
        </p>
      </div>
    </div>
  </section>
</template>

<style scoped>
.block {
  margin-top: var(--space-4);
}
.locked {
  margin-top: var(--space-2);
}
.skeleton {
  margin-top: var(--space-5);
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-6);
  justify-content: center;
  border: 1px dashed var(--c-border-strong);
  border-radius: var(--radius);
  color: var(--c-muted);
}
.stats {
  margin-top: var(--space-5);
  transition: opacity 0.15s;
}
.stats.stale .kpis,
.stats.stale .partition {
  opacity: 0.55;
}
.stale-label {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: 13px;
  color: var(--c-primary-text);
  margin-bottom: var(--space-3);
}
.kpis {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: var(--space-3);
}
.kpi {
  display: flex;
  flex-direction: column;
  padding: 14px;
  border: 1px solid var(--c-border);
  border-radius: var(--radius);
  background: var(--c-surface-2);
  min-width: 0;
}
.kpi-users {
  background: var(--c-primary-soft);
  border-color: #c7d2fe;
}
.kpi-total {
  background: var(--c-success-soft);
  border-color: #a7f3d0;
}
.kpi-label {
  font-size: 12px;
  font-weight: 600;
  color: var(--c-text-2);
}
.kpi-value {
  font-size: 26px;
  font-weight: 750;
  letter-spacing: -0.02em;
  line-height: 1.2;
  margin-top: 4px;
}
.kpi-value.muted {
  color: var(--c-muted);
}
.kpi-unit {
  font-size: 12px;
  color: var(--c-muted);
}
.partition {
  margin-top: var(--space-5);
}
.partition-title {
  font-size: 13.5px;
  font-weight: 700;
  color: var(--c-text-2);
  margin-bottom: var(--space-2);
}
.bar {
  display: flex;
  height: 12px;
  border-radius: 999px;
  overflow: hidden;
  background: #f1f5f9;
}
.seg {
  display: block;
  height: 100%;
}
.seg.eligible {
  background: var(--c-eligible);
}
.seg.no-consent {
  background: var(--c-no-consent);
}
.seg.no-contact {
  background: var(--c-no-contact);
}
.seg.both {
  background: var(--c-both);
}
.seg.sms {
  background: var(--c-sms);
}
.seg.email {
  background: var(--c-email);
}
.seg.unreachable {
  background: var(--c-unreachable);
}
.legend {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: var(--space-2);
  margin: var(--space-3) 0 0;
}
.legend.four {
  grid-template-columns: repeat(4, minmax(0, 1fr));
}
.legend dt {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 12.5px;
  color: var(--c-text-2);
}
.legend dd {
  margin: 0;
  font-weight: 700;
  font-size: 16px;
}
.identity {
  margin-top: var(--space-2);
  font-size: 12.5px;
  color: var(--c-muted);
}
.identity strong {
  color: var(--c-text-2);
}
.evaluated {
  margin-top: var(--space-5);
  padding-top: var(--space-3);
  border-top: 1px solid var(--c-border);
  font-size: 12.5px;
  color: var(--c-muted);
}
@media (max-width: 760px) {
  .kpis {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
  .legend.four {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}
</style>
