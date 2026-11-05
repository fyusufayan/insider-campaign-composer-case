// Real-shaped API payloads (field names and shapes from the CP6 DTOs).
import type {
  AttemptOutcome,
  ChannelDeliveryStats,
  DeliveryAttempts,
  DeliveryPartition,
  DeliveryReason,
  DeliveryStats,
  WorkerHealth,
  ApprovalSummary,
  AudienceStats,
  Campaign,
  Channel,
  ChannelStats,
  ContentField,
  ContentValidation,
  ContentVersion,
  CurrentContent,
  FieldError,
  LengthStats,
  SegmentRule,
} from '@/api/types'

export const CAMPAIGN_ID = '0b9c2f4e-6a1d-4c3b-9e8f-1a2b3c4d5e6f'
export const OTHER_ID = '7f1e2d3c-4b5a-4968-8776-655443322110'

export const EXAMPLE_BRIEF =
  'Send a short and friendly 30% discount reminder to users in Istanbul who added products to their cart in the last 30 days but did not complete a purchase.'

export const EXAMPLE_RULE: SegmentRule = {
  version: 1,
  match: 'all',
  conditions: [
    { type: 'attribute', field: 'city', op: 'eq', value: 'istanbul' },
    { type: 'behavior', predicate: 'abandoned_cart', within_days: 30 },
  ],
}

export const EXAMPLE_INTERPRETATION = {
  summary: 'Users in Istanbul who added a product to their cart in the last 30 days and have not purchased since.',
  assumptions: ["'Did not complete a purchase' means no purchase after the cart addition."],
  unsupported: [] as string[],
  model: 'demo',
  prompt_version: 'segment_v1',
}

export const EXAMPLE_CONTENT_BRIEF = {
  goal: 'cart_recovery',
  tone: 'friendly',
  length: 'short',
  offer: { kind: 'percent_discount', value: 30 },
}

export function draftCampaign(overrides: Partial<Campaign> = {}): Campaign {
  return {
    id: CAMPAIGN_ID,
    name: 'İstanbul cart recovery',
    brief: EXAMPLE_BRIEF,
    status: 'draft',
    channels: [],
    segment_rule: null,
    segment_version: 0,
    interpretation: null,
    content_brief: null,
    current_content_id: null,
    active_approval_id: null,
    version: 1,
    created_at: '2026-11-04T10:00:00Z',
    updated_at: '2026-11-04T10:00:00Z',
    ...overrides,
  }
}

export function segmentedCampaign(overrides: Partial<Campaign> = {}): Campaign {
  return draftCampaign({
    status: 'segmented',
    channels: ['sms', 'email'],
    segment_rule: EXAMPLE_RULE,
    segment_version: 1,
    interpretation: { ...EXAMPLE_INTERPRETATION },
    content_brief: { ...EXAMPLE_CONTENT_BRIEF },
    version: 2,
    updated_at: '2026-11-04T10:01:00Z',
    ...overrides,
  })
}

/** Per-channel partition of the 412 matched users (sums to 412 each). */
export const CHANNEL_PARTITION: Record<Channel, ChannelStats> = {
  sms: { eligible: 251, no_consent: 140, no_contact: 21 },
  email: { eligible: 318, no_consent: 82, no_contact: 12 },
}
export const MATCHED = 412
/** Users eligible on both channels. */
export const BOTH = 202

/** Audience stats consistent with §6.5 for a channel selection. */
export function audienceFor(channels: Channel[], computedAt = '2026-11-04T10:01:02Z'): AudienceStats {
  const sms = channels.includes('sms')
  const email = channels.includes('email')
  const byChannel: AudienceStats['by_channel'] = {}
  if (sms) byChannel.sms = { ...CHANNEL_PARTITION.sms }
  if (email) byChannel.email = { ...CHANNEL_PARTITION.email }
  const both = sms && email ? BOTH : 0
  const smsOnly = sms ? CHANNEL_PARTITION.sms.eligible - both : 0
  const emailOnly = email ? CHANNEL_PARTITION.email.eligible - both : 0
  return {
    matched_users: MATCHED,
    by_channel: byChannel,
    reach_users: { both, sms_only: smsOnly, email_only: emailOnly, unreachable: MATCHED - both - smsOnly - emailOnly },
    messages: (sms ? CHANNEL_PARTITION.sms.eligible : 0) + (email ? CHANNEL_PARTITION.email.eligible : 0),
    computed_at: computedAt,
  }
}

export function errorEnvelope(
  code: string,
  message: string,
  extra: { details?: unknown[]; retryable?: boolean; raw_proposal?: string } = {},
) {
  return {
    error: {
      code,
      message,
      details: extra.details ?? [],
      retryable: extra.retryable ?? false,
      ...(extra.raw_proposal !== undefined ? { raw_proposal: extra.raw_proposal } : {}),
    },
  }
}

// ---- Content (CP13 DTOs) ----

export const GEN_SMS = 'Hi {{first_name|there}}, your cart is waiting. Enjoy 30% off today.'
export const GEN_SUBJECT = '{{first_name|Hello}}, your cart is waiting: 30% off'
export const GEN_BODY = 'Hi {{first_name|there}},\n\nYour cart is still waiting. Enjoy 30% off your order.\n\nSee you soon!'
export const EXAMPLE_VALUES = { first_name: 'Ayşe', last_name: 'Yılmaz', city: 'İstanbul' }
export const CONTENT_LIMITS = { sms_body: 160, email_subject: 80, email_body: 5000 }
export const EMAIL_FOOTER =
  'You are receiving this email because you agreed to receive marketing emails from us. To stop receiving them, update your communication preferences.'

const VARS: Record<string, number> = { first_name: 20, last_name: 25, city: 13 }
const EXAMPLE: Record<string, string> = { first_name: 'Ayşe', last_name: 'Yılmaz', city: 'İstanbul' }
const PLACEHOLDER = /\{\{([^{}|]*)(?:\|([^{}]*))?\}\}/g

/**
 * A small stand-in for the backend validator (content.Parse) so the fake
 * backend returns realistic results: required fields, unknown variables,
 * worst-case length and the rendered example. Not the real rules.
 */
export function fakeValidate(
  draft: Partial<Record<ContentField, string | null>>,
  channels: Channel[],
): ContentValidation {
  const errors: FieldError[] = []
  const stats: Partial<Record<ContentField, LengthStats>> = {}
  const fields: ContentField[] = []
  if (channels.includes('sms')) fields.push('sms_body')
  if (channels.includes('email')) fields.push('email_subject', 'email_body')
  for (const f of ['sms_body', 'email_subject', 'email_body'] as ContentField[]) {
    const raw = draft[f]
    const text = (raw ?? '').normalize('NFC').trim()
    const selected = fields.includes(f)
    if (!selected) {
      if (text) errors.push({ path: f, code: 'channel_not_selected', message: 'the channel is not selected' })
      continue
    }
    if (raw === null || raw === undefined) {
      errors.push({ path: f, code: 'required', message: 'is required for the selected channel' })
      continue
    }
    if (!text) {
      errors.push({ path: f, code: 'empty', message: 'must not be empty' })
      continue
    }
    let worst = 0
    let rest = text
    for (const m of text.matchAll(PLACEHOLDER)) {
      const name = m[1]!
      if (!(name in VARS)) errors.push({ path: f, code: 'unknown_variable', message: 'unknown placeholder variable', allowed: Object.keys(VARS) })
      worst += Math.max(VARS[name] ?? 0, [...(m[2] ?? '')].length)
      rest = rest.replace(m[0], '')
    }
    worst += [...rest].length
    const limit = CONTENT_LIMITS[f]
    stats[f] = { chars_worst_case: worst, limit }
    if (worst > limit) errors.push({ path: f, code: 'too_long', message: `is longer than ${limit} characters in the worst case` })
  }
  const render = (f: ContentField) =>
    (draft[f] ?? '').normalize('NFC').trim().replace(PLACEHOLDER, (_, n: string) => EXAMPLE[n] ?? '')
  const valid = errors.length === 0
  // Mimics the advisory offer policy for manual text (30% authorized).
  const warnings: FieldError[] = valid
    ? fields
        .filter((f) => /free shipping|coupon/i.test(draft[f] ?? ''))
        .map((f) => ({ path: f, code: 'unauthorized_offer', message: 'mentions an incentive other than the authorized 30% discount' }))
    : []
  return {
    valid,
    errors,
    warnings,
    sms: stats.sms_body ?? null,
    subject: stats.email_subject ?? null,
    body: stats.email_body ?? null,
    rendered_example: valid
      ? {
          sms: fields.includes('sms_body') ? render('sms_body') : null,
          email: fields.includes('email_body')
            ? { subject: render('email_subject'), body: render('email_body'), footer: EMAIL_FOOTER }
            : null,
        }
      : null,
  }
}

export function contentVersion(overrides: Partial<ContentVersion> = {}): ContentVersion {
  return {
    id: 101,
    version: 1,
    origin: 'ai',
    model: 'demo',
    prompt_version: 'content_v1',
    created_at: '2026-11-04T10:02:00Z',
    sms_body: GEN_SMS,
    email_subject: GEN_SUBJECT,
    email_body: GEN_BODY,
    ...overrides,
  }
}

export function currentContent(c: Campaign, v: ContentVersion | null): CurrentContent {
  return {
    campaign_id: c.id,
    campaign_version: c.version,
    channels: [...c.channels],
    content: v,
    validation: v ? fakeValidate(storedForChannels(v, c.channels), c.channels) : null,
    example_values: { ...EXAMPLE_VALUES },
    limits: { ...CONTENT_LIMITS },
  }
}

/** Stored text of the selected channels only (unselected text is not validated). */
function storedForChannels(v: ContentVersion, chs: Channel[]): Partial<Record<ContentField, string | null>> {
  return {
    sms_body: chs.includes('sms') ? v.sms_body : null,
    email_subject: chs.includes('email') ? v.email_subject : null,
    email_body: chs.includes('email') ? v.email_body : null,
  }
}

// ---- Approval (CP15) ----

export const APPROVAL_ID = 'a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d'
export const DIGEST = 'ab'.repeat(32)
export const SERVER_TIME = '2026-11-04T10:20:00Z'

/** A pending approval of campaign c (which must already be pending_approval) bound to content v. */
export function approvalFor(c: Campaign, v: ContentVersion, overrides: Partial<ApprovalSummary> = {}): ApprovalSummary {
  const stats = audienceFor(c.channels)
  const snapshot: ApprovalSummary['snapshot'] = {}
  for (const ch of c.channels) snapshot[ch] = { recipients: stats.by_channel[ch]!.eligible, render_issues: {} }
  const validation = fakeValidate(storedForChannels(v, c.channels), c.channels)
  return {
    approval_id: APPROVAL_ID,
    campaign_id: c.id,
    status: 'pending',
    revoke_reason: null,
    campaign_version: c.version,
    segment_version: c.segment_version,
    content_id: v.id,
    content_version: v.version,
    content_origin: v.origin,
    content_model: v.model,
    content_prompt_version: v.prompt_version,
    channels: [...c.channels],
    audience_stats: stats,
    snapshot_count: stats.messages,
    snapshot_digest: DIGEST,
    snapshot,
    prepared_at: '2026-11-04T10:15:00Z',
    expires_at: '2026-11-04T10:30:00Z',
    approved_at: null,
    approved_by: null,
    closed_at: null,
    review: { validation, offer_findings: [], offer: { kind: 'percent_discount', value: 30 } },
    rendered_content: validation.rendered_example,
    superseded_approval_id: null,
    ...overrides,
  }
}

// ---- Delivery statistics (CP17B) ----

export interface ChannelInput {
  pending?: number
  in_flight?: number
  retry_scheduled?: number
  sent?: number
  failed?: number
  skipped_consent_revoked?: number
  skipped_no_contact?: number
  cancelled?: number
  uncertain?: number
  /** Attempts by outcome (default: one accepted attempt per sent and one permanent_error per failed). */
  attempts?: Partial<Record<AttemptOutcome, number>>
  reasons?: DeliveryReason[]
}

const OUTCOMES: AttemptOutcome[] = ['accepted', 'replayed', 'rate_limited', 'transient_error', 'timeout', 'permanent_error', 'unknown']

function attemptsOf(p: DeliveryPartition, input: ChannelInput): DeliveryAttempts {
  const by = Object.fromEntries(OUTCOMES.map((o) => [o, 0])) as Record<AttemptOutcome, number>
  const given = input.attempts ?? { accepted: p.sent, permanent_error: p.failed }
  for (const o of OUTCOMES) by[o] = given[o] ?? 0
  const total = OUTCOMES.reduce((n, o) => n + by[o], 0)
  const attempted = Math.min(total, p.sent + p.failed)
  return { total, by_outcome: by, retries: total - attempted, retried_deliveries: Math.min(total - attempted, attempted), recent: 0, per_second: 0 }
}

export function freshWorker(overrides: Partial<WorkerHealth> = {}): WorkerHealth {
  return { status: 'fresh', last_seen_at: '2026-11-04T10:30:00Z', heartbeat_age_seconds: 1.2, stale_after_seconds: 15, fresh_processes: 1, ...overrides }
}

/**
 * Consistent delivery statistics for campaign c (status, version and
 * terminal flags follow the backend's rules). channels: per-channel
 * partition input; omit for a campaign that never sent (approval_id null).
 */
export function deliveryStats(
  c: Campaign,
  opts: { channels?: Partial<Record<Channel, ChannelInput>>; approvalId?: string; worker?: WorkerHealth; updatedAt?: string } = {},
): DeliveryStats {
  const keys = ['pending', 'in_flight', 'retry_scheduled', 'sent', 'failed', 'skipped_consent_revoked', 'skipped_no_contact', 'cancelled'] as const
  const channels: Partial<Record<Channel, ChannelDeliveryStats>> = {}
  const totals: DeliveryPartition = {
    approved_total: 0, pending: 0, in_flight: 0, retry_scheduled: 0, sent: 0, failed: 0,
    skipped_consent_revoked: 0, skipped_no_contact: 0, cancelled: 0, uncertain: 0,
  }
  const allAttempts = attemptsOf(totals, { attempts: {} })
  for (const ch of ['sms', 'email'] as Channel[]) {
    const input = opts.channels?.[ch]
    if (!input) continue
    const p = { ...totals, approved_total: 0, uncertain: input.uncertain ?? 0 }
    for (const k of keys) p[k] = input[k] ?? 0
    p.approved_total = keys.reduce((n, k) => n + p[k], 0)
    const a = attemptsOf(p, input)
    channels[ch] = { ...p, attempts: a, reasons: input.reasons ?? [] }
    for (const k of [...keys, 'approved_total', 'uncertain'] as const) totals[k] += p[k]
    allAttempts.total += a.total
    allAttempts.retries += a.retries
    allAttempts.retried_deliveries += a.retried_deliveries
    for (const o of OUTCOMES) allAttempts.by_outcome[o] += a.by_outcome[o]
  }
  const processed = totals.sent + totals.failed + totals.skipped_consent_revoked + totals.skipped_no_contact + totals.cancelled
  const remaining = totals.pending + totals.in_flight + totals.retry_scheduled
  const finished = c.status === 'completed' || c.status === 'completed_with_failures' || c.status === 'cancelled'
  const terminal = finished && remaining === 0
  const sentAny = opts.channels !== undefined
  return {
    campaign_id: c.id,
    campaign_status: c.status,
    campaign_version: c.version,
    approval_id: sentAny ? (opts.approvalId ?? APPROVAL_ID) : null,
    segment_version: sentAny ? c.segment_version : null,
    content_id: sentAny ? 101 : null,
    content_version: sentAny ? 1 : null,
    channels,
    totals,
    attempts: allAttempts,
    processed,
    remaining,
    progress: totals.approved_total > 0 ? processed / totals.approved_total : 0,
    campaign_terminal: finished,
    terminal,
    outcome: terminal ? (c.status as DeliveryStats['outcome']) : finished ? 'settling' : c.status === 'sending' ? 'sending' : 'not_started',
    send_started_at: sentAny ? '2026-11-04T10:25:00Z' : null,
    completed_at: c.status === 'completed' || c.status === 'completed_with_failures' ? '2026-11-04T10:40:00Z' : null,
    cancelled_at: c.status === 'cancelled' ? '2026-11-04T10:35:00Z' : null,
    worker: opts.worker ?? freshWorker(),
    updated_at: opts.updatedAt ?? new Date().toISOString(),
  }
}
