// API types mirroring the CP6 backend DTOs exactly
// (backend/internal/httpapi/dto.go, errors.go; segment/dsl.go, audience.go;
// ai/interpretation.go). JSON property names are the backend's.

// ---- Channels and statuses ----

export type Channel = 'sms' | 'email'

/** Canonical channel order used by the backend (sms, email). */
export const CHANNELS: readonly Channel[] = ['sms', 'email']

export type CampaignStatus =
  | 'draft'
  | 'segmented'
  | 'content_ready'
  | 'pending_approval'
  | 'approved'
  | 'sending'
  | 'completed'
  | 'completed_with_failures'
  | 'cancelled'

// ---- Segment DSL v1 (normalized form, ARCHITECTURE §6.1) ----

export type Match = 'all' | 'any'

export type AttributeField =
  | 'city'
  | 'country'
  | 'gender'
  | 'age'
  | 'signup_days_ago'
  | 'last_active_days_ago'
  | 'language'

export type AttributeOp = 'eq' | 'neq' | 'in' | 'not_in' | 'gte' | 'lte' | 'between'

export interface RangeValue {
  min: number
  max: number
}

/** eq/neq → string; in/not_in → string[]; gte/lte → number; between → RangeValue. */
export type AttributeValue = string | string[] | number | RangeValue

export interface AttributeCondition {
  type: 'attribute'
  field: AttributeField
  op: AttributeOp
  value: AttributeValue
}

export type EventName = 'cart_add' | 'purchase'
export type EventOp = 'exists' | 'not_exists' | 'count_gte' | 'count_lte'

export interface EventCondition {
  type: 'event'
  event: EventName
  op: EventOp
  /** Present only for count_gte / count_lte. */
  count?: number
  within_days: number
  /** Present only when a category filter applies (non-empty, sorted). */
  category_in?: string[]
}

export type BehaviorPredicate = 'abandoned_cart'

export interface BehaviorCondition {
  type: 'behavior'
  predicate: BehaviorPredicate
  within_days: number
}

export type Condition = AttributeCondition | EventCondition | BehaviorCondition

export interface SegmentGroup {
  match: Match
  conditions: Condition[]
}

export interface SegmentRule {
  /** Always 1 in the normalized form. */
  version?: number
  match: Match
  conditions: Condition[]
  /** Omitted when there are no groups. Groups do not nest. */
  groups?: SegmentGroup[]
}

// ---- Interpretation and content brief ----

/** campaigns.interpretation (ai.Record). */
export interface InterpretationRecord {
  summary: string
  assumptions: string[]
  unsupported: string[]
  /** Model that produced the accepted output; "demo" for the demo interpreter. */
  model: string
  prompt_version: string
}

/** The model name the backend reports for demo output (llm.DemoModel). */
export const DEMO_MODEL = 'demo'

export type CopyLength = 'short' | 'medium' | 'long'

export type Offer =
  | { kind: 'none' }
  | { kind: 'free_shipping' }
  | { kind: 'percent_discount'; value: number }

/** campaigns.content_brief (ai.ContentBrief). */
export interface ContentBrief {
  goal: string
  tone: string
  length: CopyLength
  offer: Offer
}

// ---- Campaign resource (campaignDTO) ----

export interface Campaign {
  id: string
  name: string
  brief: string
  status: CampaignStatus
  /** Canonical order (sms, email); [] when none are selected. */
  channels: Channel[]
  /** Validated, normalized rule; null before the first successful interpretation. */
  segment_rule: SegmentRule | null
  segment_version: number
  interpretation: InterpretationRecord | null
  /** Kept as unknown: decoded defensively by the UI (see parseContentBrief). */
  content_brief: unknown
  /** Immutable content version the campaign points at; null without content. */
  current_content_id: number | null
  version: number
  created_at: string
  updated_at: string
}

// ---- Audience statistics (segment.AudienceStats) ----

/** Exclusive per-channel partition of matched users (units: users). */
export interface ChannelStats {
  eligible: number
  no_consent: number
  no_contact: number
}

/** Matched users by the selected channels they are eligible on (units: users). */
export interface ReachStats {
  both: number
  sms_only: number
  email_only: number
  unreachable: number
}

export interface AudienceStats {
  matched_users: number
  /** Exactly the selected channels; {} when none are selected. */
  by_channel: Partial<Record<Channel, ChannelStats>>
  reach_users: ReachStats
  /** Σ eligible over the selected channels (units: messages). */
  messages: number
  computed_at: string
}

// ---- Responses ----

/** POST /campaigns/{id}/interpret. */
export interface InterpretResponse {
  campaign: Campaign
  /** null when the audience could not be computed after the interpretation was saved. */
  audience: AudienceStats | null
  audience_error?: ErrorBody
}

/** GET /campaigns/{id}/audience. */
export interface AudienceResponse {
  campaign_id: string
  campaign_version: number
  segment_version: number
  channels: Channel[]
  audience: AudienceStats
}

/** GET /readyz (served outside /api/v1). */
export interface ReadinessReport {
  status: 'ready' | 'not_ready'
  database: string
  migration_version: number
  migration_dirty: boolean
  llm_provider: string
}

// ---- Content (CP13; backend httpapi/dto.go content DTOs) ----

/** Draft field names (content.Draft DTO, campaign_contents columns). */
export type ContentField = 'sms_body' | 'email_subject' | 'email_body'

export const CONTENT_FIELDS: readonly ContentField[] = ['sms_body', 'email_subject', 'email_body']

/** Fields required by each channel. */
export const CHANNEL_FIELDS: Record<Channel, readonly ContentField[]> = {
  sms: ['sms_body'],
  email: ['email_subject', 'email_body'],
}

export type ContentOrigin = 'ai' | 'edited'

/** One immutable content version. Text is canonical (NFC, trimmed). */
export interface ContentVersion {
  id: number
  version: number
  origin: ContentOrigin
  /** Set for AI versions ("demo" for the demo generator). */
  model: string | null
  prompt_version: string | null
  created_at: string
  sms_body: string | null
  email_subject: string | null
  email_body: string | null
}

/** Worst-case length after personalization (runes after NFC). */
export interface LengthStats {
  chars_worst_case: number
  limit: number
}

export interface RenderedEmail {
  subject: string
  body: string
  /** Fixed footer appended by the backend (outside the body budget). */
  footer: string
}

/** Rendered example with the fixed fictional example values. */
export interface RenderedExample {
  sms: string | null
  email: RenderedEmail | null
}

/** content.Result for the campaign's selected channels. */
export interface ContentValidation {
  valid: boolean
  errors: FieldError[]
  warnings: FieldError[]
  sms: LengthStats | null
  subject: LengthStats | null
  body: LengthStats | null
  /** null when there are errors. */
  rendered_example: RenderedExample | null
}

/** Fixed, fictional preview values (never customer data). */
export interface ExampleValues {
  first_name: string
  last_name: string
  city: string
}

export interface ContentLimits {
  sms_body: number
  email_subject: number
  email_body: number
}

/** GET /campaigns/{id}/content. content and validation are null without content. */
export interface CurrentContent {
  campaign_id: string
  campaign_version: number
  channels: Channel[]
  content: ContentVersion | null
  validation: ContentValidation | null
  example_values: ExampleValues
  limits: ContentLimits
}

/** POST /campaigns/{id}/content/validate (read-only). */
export interface DraftValidation {
  campaign_id: string
  campaign_version: number
  channels: Channel[]
  validation: ContentValidation
  example_values: ExampleValues
  limits: ContentLimits
}

/** PUT /campaigns/{id}/content and the generation stream's "validated" event. */
export interface ContentSaved {
  campaign: Campaign
  content: CurrentContent
  /** false: the canonical text was unchanged and nothing was written. */
  created: boolean
  notes: string[]
}

/** A content draft: null = field not provided. */
export interface ContentDraft {
  sms_body: string | null
  email_subject: string | null
  email_body: string | null
}

export interface SaveContentRequest extends ContentDraft {
  expected_version: number
}

// ---- Generation stream (POST /content/generations, text/event-stream) ----

export type GenerationPhase = 'generating' | 'thinking' | 'validating' | 'repairing' | 'persisting'

export interface GenerationMeta {
  campaign_id: string
  campaign_version: number
  channels: Channel[]
  fields: ContentField[]
  prompt_version: string
  limits: ContentLimits
}

export interface GenerationStatus {
  phase: GenerationPhase
  attempt?: number
  reason?: 'initial' | 'truncation_retry' | 'repair'
  problems?: number
}

/** Unvalidated preview fragment of one field of one attempt. */
export interface GenerationDelta {
  attempt: number
  field: ContentField
  text: string
  preview: true
}

export interface GenerationErrorEvent {
  status: number
  error: ErrorBody
}

export type GenerationEvent =
  | { type: 'meta'; data: GenerationMeta }
  | { type: 'status'; data: GenerationStatus }
  | { type: 'delta'; data: GenerationDelta }
  | { type: 'validated'; data: ContentSaved }
  | { type: 'error'; data: GenerationErrorEvent }

// ---- Requests ----

export interface CreateCampaignRequest {
  name: string
  brief: string
}

export interface SetChannelsRequest {
  channels: Channel[]
  expected_version: number
}

// ---- Error envelope ----

export interface FieldError {
  path: string
  code: string
  message: string
  allowed?: string[]
}

export type ErrorCode =
  | 'validation_failed'
  | 'not_found'
  | 'method_not_allowed'
  | 'internal'
  | 'unsupported_media_type'
  | 'invalid_state'
  | 'version_conflict'
  | 'audience_unavailable'
  | 'request_cancelled'
  | 'ai_output_invalid'
  | 'llm_unavailable'
  | 'llm_rejected'
  | 'llm_refused'
  | 'llm_truncated'
  | 'invalid_brief'

export interface ErrorBody {
  /** One of ErrorCode for this API version; unknown codes are tolerated. */
  code: string
  message: string
  details: FieldError[]
  retryable: boolean
  /** Only for ai_output_invalid. Untrusted model output: display escaped. */
  raw_proposal?: string
}

export interface ErrorEnvelope {
  error: ErrorBody
}

// ---- Field limits (backend campaign.NewDraft; runes after NFC + trim) ----

export const LIMITS = {
  nameMin: 1,
  nameMax: 120,
  briefMin: 10,
  briefMax: 2000,
} as const
