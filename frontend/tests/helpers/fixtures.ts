// Real-shaped API payloads (field names and shapes from the CP6 DTOs).
import type { AudienceStats, Campaign, Channel, ChannelStats, SegmentRule } from '@/api/types'

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
