<script setup lang="ts">
// The frozen approval as the server reported it: binding (content version,
// segment version, channels, snapshot digest), the exact partition stored
// with the approval, the frozen recipients per channel, render issues,
// timestamps and the rendered content (fictional example values). Counts
// only — the browser never receives recipient records. All text is
// interpolated (escaped); nothing is rendered as HTML.
import { computed } from 'vue'
import type { ApprovalSummary, Channel, FieldError } from '@/api/types'
import { DEMO_MODEL } from '@/api/types'
import { CHANNEL_LABEL, formatCount, formatDateTime } from '@/composables/format'
import EmailPreview from './EmailPreview.vue'
import SmsPreview from './SmsPreview.vue'

const props = defineProps<{ approval: ApprovalSummary; remaining: string }>()

const OFFER_CODES = ['unauthorized_offer', 'unauthorized_claim']
const RENDER_ISSUE_LABEL: Record<string, string> = {
  missing_value: 'missing personalization value and no fallback',
  invalid_value: 'invalid personalization value',
  value_too_long: 'personalization value longer than allowed',
  content_too_long: 'message would exceed the length limit',
}

const a = computed(() => props.approval)
const channels = computed<Channel[]>(() => a.value.channels)
const live = computed(() => a.value.status === 'pending' || a.value.status === 'approved')
const stats = computed(() => a.value.audience_stats)
const otherWarnings = computed<FieldError[]>(() =>
  (a.value.review?.validation.warnings ?? []).filter((w) => !OFFER_CODES.includes(w.code)),
)
const renderIssues = computed(() => {
  const out: { channel: Channel; code: string; count: number }[] = []
  for (const ch of channels.value) {
    const issues = a.value.snapshot[ch]?.render_issues ?? {}
    for (const [code, count] of Object.entries(issues)) if (count > 0) out.push({ channel: ch, code, count })
  }
  return out
})
const contentLabel = computed(() => {
  if (a.value.content_origin === 'edited') return 'edited manually'
  if (a.value.content_model === DEMO_MODEL) return 'AI (demo generator, not Claude)'
  return `AI (${a.value.content_model ?? 'model unknown'}${a.value.content_prompt_version ? `, ${a.value.content_prompt_version}` : ''})`
})
const shortDigest = computed(() => a.value.snapshot_digest.slice(0, 12))
const rendered = computed(() => a.value.rendered_content)
const STATUS_TEXT: Record<string, string> = {
  pending: 'Prepared — awaiting your confirmation',
  approved: 'Confirmed',
  consumed: 'Used for sending',
  revoked: 'Withdrawn or replaced',
  expired: 'Expired',
}
</script>

<template>
  <div class="summary" data-testid="approval-summary">
    <dl class="facts">
      <div>
        <dt>Approval</dt>
        <dd data-testid="approval-status">{{ STATUS_TEXT[a.status] ?? a.status }}</dd>
      </div>
      <div>
        <dt>Channels</dt>
        <dd data-testid="approval-channels">{{ channels.map((c) => CHANNEL_LABEL[c]).join(' + ') }}</dd>
      </div>
      <div>
        <dt>Content</dt>
        <dd data-testid="approval-content">Version {{ a.content_version }} · {{ contentLabel }}</dd>
      </div>
      <div>
        <dt>Audience rule</dt>
        <dd data-testid="approval-segment">Segment version {{ a.segment_version }}</dd>
      </div>
      <div>
        <dt>Prepared</dt>
        <dd>{{ formatDateTime(a.prepared_at) }}</dd>
      </div>
      <div v-if="a.approved_at">
        <dt>Confirmed</dt>
        <dd data-testid="approval-approved-at">{{ formatDateTime(a.approved_at) }}<span v-if="a.approved_by"> by {{ a.approved_by }}</span></dd>
      </div>
      <div v-if="live">
        <dt>{{ a.status === 'pending' ? 'Confirm before' : 'Valid until' }}</dt>
        <dd data-testid="approval-expiry">
          {{ formatDateTime(a.expires_at) }} <span class="remaining">({{ remaining }})</span>
        </dd>
      </div>
      <div>
        <dt>Snapshot</dt>
        <dd>
          <code class="mono" :title="a.snapshot_digest" data-testid="approval-digest">{{ shortDigest }}…</code>
        </dd>
      </div>
    </dl>

    <div class="table-wrap">
      <table class="audience" data-testid="approval-audience">
        <caption>
          Frozen audience ·
          <strong data-testid="approval-total">{{ formatCount(a.snapshot_count) }}</strong>
          prepared {{ a.snapshot_count === 1 ? 'delivery' : 'deliveries' }} from
          {{ formatCount(stats.matched_users) }} matched users
        </caption>
        <thead>
          <tr>
            <th scope="col">Channel</th>
            <th scope="col" class="num">Frozen recipients</th>
            <th scope="col" class="num">No consent</th>
            <th scope="col" class="num">No contact</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="ch in channels" :key="ch" :data-testid="`approval-row-${ch}`">
            <th scope="row">{{ CHANNEL_LABEL[ch] }}</th>
            <td class="num">{{ formatCount(a.snapshot[ch]?.recipients ?? 0) }}</td>
            <td class="num">{{ formatCount(stats.by_channel[ch]?.no_consent ?? 0) }}</td>
            <td class="num">{{ formatCount(stats.by_channel[ch]?.no_contact ?? 0) }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <p class="reach" data-testid="approval-reach">
      Reach: {{ formatCount(stats.reach_users.both) }} on both channels ·
      {{ formatCount(stats.reach_users.sms_only) }} SMS only · {{ formatCount(stats.reach_users.email_only) }} email only ·
      {{ formatCount(stats.reach_users.unreachable) }} unreachable
    </p>

    <div v-if="renderIssues.length" class="notice notice-warning" role="note" data-testid="approval-render-issues">
      <div>
        <p class="notice-title">Some frozen recipients cannot be personalized</p>
        <ul>
          <li v-for="r in renderIssues" :key="r.channel + r.code">
            {{ CHANNEL_LABEL[r.channel] }}: {{ formatCount(r.count) }} — {{ RENDER_ISSUE_LABEL[r.code] ?? r.code }}
          </li>
        </ul>
        <p>These deliveries will fail permanently instead of being sent with wrong or truncated text.</p>
      </div>
    </div>

    <div v-if="otherWarnings.length" class="warnings" data-testid="approval-warnings">
      <p class="label">Content warnings (non-blocking)</p>
      <ul>
        <li v-for="(w, i) in otherWarnings" :key="i">
          <span v-if="w.path" class="mono">{{ w.path }}</span> {{ w.message }}
        </li>
      </ul>
    </div>

    <div v-if="rendered" class="previews" data-testid="approval-previews">
      <p class="label">Frozen content, rendered for a fictional example recipient</p>
      <div class="preview-grid" :class="{ two: channels.length === 2 }">
        <SmsPreview v-if="channels.includes('sms')" :text="rendered.sms" unavailable="No SMS text." />
        <EmailPreview v-if="channels.includes('email')" :email="rendered.email" unavailable="No email text." />
      </div>
    </div>
  </div>
</template>

<style scoped>
.summary {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  min-width: 0;
}
.facts {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(190px, 1fr));
  gap: var(--space-3) var(--space-5);
  margin: 0;
}
.facts dt {
  font-size: 12px;
  color: var(--c-muted);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}
.facts dd {
  margin: 2px 0 0;
  overflow-wrap: anywhere;
}
.remaining {
  color: var(--c-muted);
}
.table-wrap {
  overflow-x: auto;
}
.audience {
  width: 100%;
  border-collapse: collapse;
  font-size: 14px;
}
.audience caption {
  text-align: left;
  padding-bottom: var(--space-2);
  color: var(--c-text-2);
}
.audience th,
.audience td {
  padding: 6px 8px;
  border-bottom: 1px solid var(--c-border, #e2e8f0);
  text-align: left;
}
.audience th.num,
.audience .num {
  text-align: right;
  font-variant-numeric: tabular-nums;
}
.reach {
  font-size: 13px;
  color: var(--c-muted);
}
.label {
  font-weight: 600;
  margin-bottom: var(--space-2);
}
.warnings ul,
.notice ul {
  margin: 4px 0;
  padding-left: 18px;
}
.preview-grid {
  display: grid;
  gap: var(--space-4);
}
.preview-grid.two {
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
}
@media (max-width: 720px) {
  .preview-grid.two {
    grid-template-columns: 1fr;
  }
}
</style>
