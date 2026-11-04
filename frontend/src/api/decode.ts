// Defensive runtime decoding of API responses. TypeScript types are erased at
// runtime, so every response body is checked before the UI trusts its shape.
// A body that does not match becomes an `invalid_response` ApiError instead of
// an exception deep inside a component.
//
// The segment rule is checked structurally only (match/conditions/groups);
// the condition presenter handles unknown condition shapes explicitly
// (it never invents a description for something it does not recognize).

import type {
  AudienceResponse,
  AudienceStats,
  Campaign,
  CampaignStatus,
  Channel,
  ChannelStats,
  ContentBrief,
  ErrorBody,
  FieldError,
  InterpretResponse,
  InterpretationRecord,
  ReachStats,
  ReadinessReport,
  SegmentRule,
} from './types'

export class DecodeError extends Error {
  constructor(readonly path: string, expected: string) {
    super(`${path}: expected ${expected}`)
    this.name = 'DecodeError'
  }
}

type Obj = Record<string, unknown>

function isObj(v: unknown): v is Obj {
  return typeof v === 'object' && v !== null && !Array.isArray(v)
}

function obj(v: unknown, path: string): Obj {
  if (!isObj(v)) throw new DecodeError(path, 'an object')
  return v
}

function str(v: unknown, path: string): string {
  if (typeof v !== 'string') throw new DecodeError(path, 'a string')
  return v
}

function int(v: unknown, path: string): number {
  if (typeof v !== 'number' || !Number.isInteger(v)) throw new DecodeError(path, 'an integer')
  return v
}

function bool(v: unknown, path: string): boolean {
  if (typeof v !== 'boolean') throw new DecodeError(path, 'a boolean')
  return v
}

function strArray(v: unknown, path: string): string[] {
  if (!Array.isArray(v)) throw new DecodeError(path, 'an array of strings')
  return v.map((x, i) => str(x, `${path}[${i}]`))
}

const STATUSES: readonly CampaignStatus[] = [
  'draft',
  'segmented',
  'content_ready',
  'pending_approval',
  'approved',
  'sending',
  'completed',
  'completed_with_failures',
  'cancelled',
]

function channel(v: unknown, path: string): Channel {
  if (v !== 'sms' && v !== 'email') throw new DecodeError(path, '"sms" or "email"')
  return v
}

function channels(v: unknown, path: string): Channel[] {
  if (!Array.isArray(v)) throw new DecodeError(path, 'an array of channels')
  return v.map((x, i) => channel(x, `${path}[${i}]`))
}

function segmentRule(v: unknown, path: string): SegmentRule | null {
  if (v === null) return null
  const o = obj(v, path)
  if (o.match !== 'all' && o.match !== 'any') throw new DecodeError(`${path}.match`, '"all" or "any"')
  if (!Array.isArray(o.conditions)) throw new DecodeError(`${path}.conditions`, 'an array')
  o.conditions.forEach((c, i) => obj(c, `${path}.conditions[${i}]`))
  if (o.groups !== undefined) {
    if (!Array.isArray(o.groups)) throw new DecodeError(`${path}.groups`, 'an array')
    o.groups.forEach((g, i) => {
      const go = obj(g, `${path}.groups[${i}]`)
      if (go.match !== 'all' && go.match !== 'any') {
        throw new DecodeError(`${path}.groups[${i}].match`, '"all" or "any"')
      }
      if (!Array.isArray(go.conditions)) throw new DecodeError(`${path}.groups[${i}].conditions`, 'an array')
      go.conditions.forEach((c, j) => obj(c, `${path}.groups[${i}].conditions[${j}]`))
    })
  }
  // Conditions are passed through as-is; the presenter recognizes known
  // shapes and labels anything else as unrecognized.
  return o as unknown as SegmentRule
}

function interpretation(v: unknown, path: string): InterpretationRecord | null {
  if (v === null) return null
  const o = obj(v, path)
  return {
    summary: str(o.summary, `${path}.summary`),
    assumptions: strArray(o.assumptions, `${path}.assumptions`),
    unsupported: strArray(o.unsupported, `${path}.unsupported`),
    model: str(o.model, `${path}.model`),
    prompt_version: str(o.prompt_version, `${path}.prompt_version`),
  }
}

export function decodeCampaign(v: unknown, path = 'campaign'): Campaign {
  const o = obj(v, path)
  const status = str(o.status, `${path}.status`)
  if (!(STATUSES as readonly string[]).includes(status)) throw new DecodeError(`${path}.status`, 'a known status')
  return {
    id: str(o.id, `${path}.id`),
    name: str(o.name, `${path}.name`),
    brief: str(o.brief, `${path}.brief`),
    status: status as CampaignStatus,
    channels: channels(o.channels, `${path}.channels`),
    segment_rule: segmentRule(o.segment_rule, `${path}.segment_rule`),
    segment_version: int(o.segment_version, `${path}.segment_version`),
    interpretation: interpretation(o.interpretation, `${path}.interpretation`),
    content_brief: o.content_brief ?? null,
    version: int(o.version, `${path}.version`),
    created_at: str(o.created_at, `${path}.created_at`),
    updated_at: str(o.updated_at, `${path}.updated_at`),
  }
}

function channelStats(v: unknown, path: string): ChannelStats {
  const o = obj(v, path)
  return {
    eligible: int(o.eligible, `${path}.eligible`),
    no_consent: int(o.no_consent, `${path}.no_consent`),
    no_contact: int(o.no_contact, `${path}.no_contact`),
  }
}

function reachStats(v: unknown, path: string): ReachStats {
  const o = obj(v, path)
  return {
    both: int(o.both, `${path}.both`),
    sms_only: int(o.sms_only, `${path}.sms_only`),
    email_only: int(o.email_only, `${path}.email_only`),
    unreachable: int(o.unreachable, `${path}.unreachable`),
  }
}

export function decodeAudienceStats(v: unknown, path = 'audience'): AudienceStats {
  const o = obj(v, path)
  const byChannelRaw = obj(o.by_channel, `${path}.by_channel`)
  const byChannel: AudienceStats['by_channel'] = {}
  for (const [k, val] of Object.entries(byChannelRaw)) {
    byChannel[channel(k, `${path}.by_channel key`)] = channelStats(val, `${path}.by_channel.${k}`)
  }
  return {
    matched_users: int(o.matched_users, `${path}.matched_users`),
    by_channel: byChannel,
    reach_users: reachStats(o.reach_users, `${path}.reach_users`),
    messages: int(o.messages, `${path}.messages`),
    computed_at: str(o.computed_at, `${path}.computed_at`),
  }
}

function fieldError(v: unknown, path: string): FieldError {
  const o = obj(v, path)
  const fe: FieldError = {
    path: typeof o.path === 'string' ? o.path : '',
    code: str(o.code, `${path}.code`),
    message: str(o.message, `${path}.message`),
  }
  if (o.allowed !== undefined) fe.allowed = strArray(o.allowed, `${path}.allowed`)
  return fe
}

export function decodeErrorBody(v: unknown, path = 'error'): ErrorBody {
  const o = obj(v, path)
  const details = o.details === undefined || o.details === null ? [] : o.details
  if (!Array.isArray(details)) throw new DecodeError(`${path}.details`, 'an array')
  const body: ErrorBody = {
    code: str(o.code, `${path}.code`),
    message: str(o.message, `${path}.message`),
    details: details.map((d, i) => fieldError(d, `${path}.details[${i}]`)),
    retryable: o.retryable === undefined ? false : bool(o.retryable, `${path}.retryable`),
  }
  if (typeof o.raw_proposal === 'string') body.raw_proposal = o.raw_proposal
  return body
}

export function decodeInterpretResponse(v: unknown): InterpretResponse {
  const o = obj(v, 'response')
  const res: InterpretResponse = {
    campaign: decodeCampaign(o.campaign, 'campaign'),
    audience: o.audience === null || o.audience === undefined ? null : decodeAudienceStats(o.audience),
  }
  if (o.audience_error !== undefined && o.audience_error !== null) {
    res.audience_error = decodeErrorBody(o.audience_error, 'audience_error')
  }
  return res
}

export function decodeAudienceResponse(v: unknown): AudienceResponse {
  const o = obj(v, 'response')
  return {
    campaign_id: str(o.campaign_id, 'campaign_id'),
    campaign_version: int(o.campaign_version, 'campaign_version'),
    segment_version: int(o.segment_version, 'segment_version'),
    channels: channels(o.channels, 'channels'),
    audience: decodeAudienceStats(o.audience),
  }
}

export function decodeReadiness(v: unknown): ReadinessReport {
  const o = obj(v, 'readiness')
  return {
    status: o.status === 'ready' ? 'ready' : 'not_ready',
    database: typeof o.database === 'string' ? o.database : '',
    migration_version: typeof o.migration_version === 'number' ? o.migration_version : 0,
    migration_dirty: o.migration_dirty === true,
    llm_provider: typeof o.llm_provider === 'string' ? o.llm_provider : '',
  }
}

/**
 * Decodes the persisted content brief (campaigns.content_brief). Returns null
 * for absent or unrecognized shapes rather than guessing.
 */
export function parseContentBrief(v: unknown): ContentBrief | null {
  if (!isObj(v)) return null
  const { goal, tone, length, offer } = v
  if (typeof goal !== 'string' || typeof tone !== 'string') return null
  if (length !== 'short' && length !== 'medium' && length !== 'long') return null
  if (!isObj(offer)) return null
  if (offer.kind === 'none' || offer.kind === 'free_shipping') {
    return { goal, tone, length, offer: { kind: offer.kind } }
  }
  if (offer.kind === 'percent_discount' && typeof offer.value === 'number' && Number.isInteger(offer.value)) {
    return { goal, tone, length, offer: { kind: 'percent_discount', value: offer.value } }
  }
  return null
}
