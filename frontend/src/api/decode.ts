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
  ContentField,
  ContentLimits,
  ContentSaved,
  ContentValidation,
  ContentVersion,
  CurrentContent,
  DraftValidation,
  ErrorBody,
  ExampleValues,
  FieldError,
  GenerationDelta,
  GenerationErrorEvent,
  GenerationEvent,
  GenerationMeta,
  GenerationPhase,
  GenerationStatus,
  InterpretResponse,
  InterpretationRecord,
  LengthStats,
  ReachStats,
  ReadinessReport,
  RenderedExample,
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

function nullableInt(v: unknown, path: string): number | null {
  // Absent is tolerated for older payloads; present must be int or null.
  if (v === null || v === undefined) return null
  return int(v, path)
}

function nullableStr(v: unknown, path: string): string | null {
  if (v === null) return null
  return str(v, path)
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
    current_content_id: nullableInt(o.current_content_id, `${path}.current_content_id`),
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

// ---- Content (CP13) ----

const CONTENT_FIELD_NAMES: readonly ContentField[] = ['sms_body', 'email_subject', 'email_body']

function contentField(v: unknown, path: string): ContentField {
  if (typeof v !== 'string' || !(CONTENT_FIELD_NAMES as readonly string[]).includes(v)) {
    throw new DecodeError(path, 'a content field name')
  }
  return v as ContentField
}

function fieldErrors(v: unknown, path: string): FieldError[] {
  if (!Array.isArray(v)) throw new DecodeError(path, 'an array')
  return v.map((d, i) => fieldError(d, `${path}[${i}]`))
}

function lengthStats(v: unknown, path: string): LengthStats | null {
  if (v === null || v === undefined) return null
  const o = obj(v, path)
  return { chars_worst_case: int(o.chars_worst_case, `${path}.chars_worst_case`), limit: int(o.limit, `${path}.limit`) }
}

function renderedExample(v: unknown, path: string): RenderedExample | null {
  if (v === null || v === undefined) return null
  const o = obj(v, path)
  let email: RenderedExample['email'] = null
  if (o.email !== null && o.email !== undefined) {
    const e = obj(o.email, `${path}.email`)
    email = {
      subject: str(e.subject, `${path}.email.subject`),
      body: str(e.body, `${path}.email.body`),
      footer: str(e.footer, `${path}.email.footer`),
    }
  }
  return { sms: o.sms === undefined ? null : nullableStr(o.sms, `${path}.sms`), email }
}

export function decodeContentValidation(v: unknown, path = 'validation'): ContentValidation {
  const o = obj(v, path)
  const res: ContentValidation = {
    valid: bool(o.valid, `${path}.valid`),
    errors: fieldErrors(o.errors, `${path}.errors`),
    warnings: fieldErrors(o.warnings, `${path}.warnings`),
    sms: lengthStats(o.sms, `${path}.sms`),
    subject: lengthStats(o.subject, `${path}.subject`),
    body: lengthStats(o.body, `${path}.body`),
    rendered_example: renderedExample(o.rendered_example, `${path}.rendered_example`),
  }
  // Consistency of the contract: valid ⇔ no errors; examples only when valid.
  if (res.valid !== (res.errors.length === 0)) throw new DecodeError(`${path}.valid`, 'valid iff errors is empty')
  if (!res.valid && res.rendered_example !== null) throw new DecodeError(`${path}.rendered_example`, 'null when invalid')
  return res
}

export function decodeContentVersion(v: unknown, path = 'content'): ContentVersion {
  const o = obj(v, path)
  const origin = o.origin
  if (origin !== 'ai' && origin !== 'edited') throw new DecodeError(`${path}.origin`, '"ai" or "edited"')
  const cv: ContentVersion = {
    id: int(o.id, `${path}.id`),
    version: int(o.version, `${path}.version`),
    origin,
    model: nullableStr(o.model, `${path}.model`),
    prompt_version: nullableStr(o.prompt_version, `${path}.prompt_version`),
    created_at: str(o.created_at, `${path}.created_at`),
    sms_body: nullableStr(o.sms_body, `${path}.sms_body`),
    email_subject: nullableStr(o.email_subject, `${path}.email_subject`),
    email_body: nullableStr(o.email_body, `${path}.email_body`),
  }
  if (cv.version < 1) throw new DecodeError(`${path}.version`, 'a positive integer')
  if (origin === 'ai' && (cv.model === null || cv.prompt_version === null)) {
    throw new DecodeError(`${path}.model`, 'model and prompt_version for AI content')
  }
  if (origin === 'edited' && (cv.model !== null || cv.prompt_version !== null)) {
    throw new DecodeError(`${path}.model`, 'null model and prompt_version for edited content')
  }
  return cv
}

function exampleValues(v: unknown, path: string): ExampleValues {
  const o = obj(v, path)
  return {
    first_name: str(o.first_name, `${path}.first_name`),
    last_name: str(o.last_name, `${path}.last_name`),
    city: str(o.city, `${path}.city`),
  }
}

function contentLimits(v: unknown, path: string): ContentLimits {
  const o = obj(v, path)
  return {
    sms_body: int(o.sms_body, `${path}.sms_body`),
    email_subject: int(o.email_subject, `${path}.email_subject`),
    email_body: int(o.email_body, `${path}.email_body`),
  }
}

export function decodeCurrentContent(v: unknown, path = 'current_content'): CurrentContent {
  const o = obj(v, path)
  const res: CurrentContent = {
    campaign_id: str(o.campaign_id, `${path}.campaign_id`),
    campaign_version: int(o.campaign_version, `${path}.campaign_version`),
    channels: channels(o.channels, `${path}.channels`),
    content: o.content === null ? null : decodeContentVersion(o.content, `${path}.content`),
    validation: o.validation === null ? null : decodeContentValidation(o.validation, `${path}.validation`),
    example_values: exampleValues(o.example_values, `${path}.example_values`),
    limits: contentLimits(o.limits, `${path}.limits`),
  }
  if ((res.content === null) !== (res.validation === null)) {
    throw new DecodeError(`${path}.validation`, 'present exactly when content is present')
  }
  return res
}

export function decodeDraftValidation(v: unknown): DraftValidation {
  const o = obj(v, 'response')
  return {
    campaign_id: str(o.campaign_id, 'campaign_id'),
    campaign_version: int(o.campaign_version, 'campaign_version'),
    channels: channels(o.channels, 'channels'),
    validation: decodeContentValidation(o.validation),
    example_values: exampleValues(o.example_values, 'example_values'),
    limits: contentLimits(o.limits, 'limits'),
  }
}

export function decodeContentSaved(v: unknown, path = 'response'): ContentSaved {
  const o = obj(v, path)
  const res: ContentSaved = {
    campaign: decodeCampaign(o.campaign, `${path}.campaign`),
    content: decodeCurrentContent(o.content, `${path}.content`),
    created: bool(o.created, `${path}.created`),
    notes: strArray(o.notes, `${path}.notes`),
  }
  if (res.content.content === null) throw new DecodeError(`${path}.content.content`, 'the saved version')
  if (res.content.campaign_id !== res.campaign.id || res.content.campaign_version !== res.campaign.version) {
    throw new DecodeError(`${path}.content.campaign_version`, 'the saved campaign version')
  }
  if (res.campaign.current_content_id !== res.content.content.id) {
    throw new DecodeError(`${path}.campaign.current_content_id`, 'the saved content id')
  }
  return res
}

const PHASES: readonly GenerationPhase[] = ['generating', 'thinking', 'validating', 'repairing', 'persisting']

/** Max text of one streamed delta (the server splits at 2 KiB). */
const MAX_DELTA_CHARS = 4096

/**
 * Decodes one event of the generation stream by its name. Unknown event
 * names are rejected (the contract is closed).
 */
export function decodeGenerationEvent(name: string, data: unknown): GenerationEvent {
  switch (name) {
    case 'meta': {
      const o = obj(data, 'meta')
      const meta: GenerationMeta = {
        campaign_id: str(o.campaign_id, 'meta.campaign_id'),
        campaign_version: int(o.campaign_version, 'meta.campaign_version'),
        channels: channels(o.channels, 'meta.channels'),
        fields: Array.isArray(o.fields)
          ? o.fields.map((f, i) => contentField(f, `meta.fields[${i}]`))
          : (() => {
              throw new DecodeError('meta.fields', 'an array')
            })(),
        prompt_version: str(o.prompt_version, 'meta.prompt_version'),
        limits: contentLimits(o.limits, 'meta.limits'),
      }
      return { type: 'meta', data: meta }
    }
    case 'status': {
      const o = obj(data, 'status')
      const phase = str(o.phase, 'status.phase')
      if (!(PHASES as readonly string[]).includes(phase)) throw new DecodeError('status.phase', 'a known phase')
      const st: GenerationStatus = { phase: phase as GenerationPhase }
      if (o.attempt !== undefined) st.attempt = int(o.attempt, 'status.attempt')
      if (o.reason !== undefined) {
        if (o.reason !== 'initial' && o.reason !== 'truncation_retry' && o.reason !== 'repair') {
          throw new DecodeError('status.reason', 'a known reason')
        }
        st.reason = o.reason
      }
      if (o.problems !== undefined) st.problems = int(o.problems, 'status.problems')
      return { type: 'status', data: st }
    }
    case 'delta': {
      const o = obj(data, 'delta')
      if (o.preview !== true) throw new DecodeError('delta.preview', 'true')
      const text = str(o.text, 'delta.text')
      if (text.length > MAX_DELTA_CHARS) throw new DecodeError('delta.text', 'a bounded fragment')
      const d: GenerationDelta = {
        attempt: int(o.attempt, 'delta.attempt'),
        field: contentField(o.field, 'delta.field'),
        text,
        preview: true,
      }
      return { type: 'delta', data: d }
    }
    case 'validated':
      return { type: 'validated', data: decodeContentSaved(data, 'validated') }
    case 'error': {
      const o = obj(data, 'error_event')
      const ev: GenerationErrorEvent = {
        status: int(o.status, 'error_event.status'),
        error: decodeErrorBody(o.error, 'error_event.error'),
      }
      return { type: 'error', data: ev }
    }
    default:
      throw new DecodeError('event', 'a known event name')
  }
}
