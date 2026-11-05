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
  /** The live (pending/approved) approval, or the consumed one once sending; null otherwise. */
  active_approval_id: string | null
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
  /** Freshest worker heartbeat (CP17B); reported only, never a readiness failure. Null if absent from the body. */
  worker: WorkerHealth | null
  sending_campaigns: number | null
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
  | 'approval_expired'
  | 'approval_mismatch'
  | 'empty_audience'
  | 'audience_changed'
  | 'campaign_busy'
  | 'too_many_streams'

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

// ---- Approval (CP15, backend httpapi/approvals.go approvalDTO) ----

export type ApprovalStatus = 'pending' | 'approved' | 'consumed' | 'revoked' | 'expired'
export type RevokeReason = 'superseded' | 'edited' | 'withdrawn' | 'campaign_cancelled'

/** Frozen recipients of one channel (counts only, never recipients). */
export interface ApprovalChannelSnapshot {
  recipients: number
  /** Held rows whose frozen values cannot be rendered, by code; null for a closed approval. */
  render_issues: Record<string, number> | null
}

/** Re-validation of the bound content against the current content brief (live approvals). */
export interface ApprovalReview {
  validation: ContentValidation
  /** Advisory keyword findings (unauthorized_offer / unauthorized_claim); never blocking for marketer text. */
  offer_findings: FieldError[]
  offer: Offer
}

export interface ApprovalSummary {
  approval_id: string
  campaign_id: string
  status: ApprovalStatus
  revoke_reason: RevokeReason | null
  campaign_version: number
  segment_version: number
  content_id: number
  content_version: number
  content_origin: ContentOrigin
  content_model: string | null
  content_prompt_version: string | null
  channels: Channel[]
  audience_stats: AudienceStats
  snapshot_count: number
  snapshot_digest: string
  snapshot: Partial<Record<Channel, ApprovalChannelSnapshot>>
  prepared_at: string
  expires_at: string
  approved_at: string | null
  approved_by: string | null
  closed_at: string | null
  /** null for a closed approval. */
  review: ApprovalReview | null
  /** The documented prepare field: review.validation.rendered_example (fictional values). */
  rendered_content: RenderedExample | null
  superseded_approval_id: string | null
}

/** Response of every approval endpoint. */
export interface ApprovalResponse {
  campaign: Campaign
  approval: ApprovalSummary
  /** Server clock at response time (aligns the expiry countdown). */
  server_time: string
}

export interface PrepareApprovalRequest {
  expected_version: number
}

export interface ConfirmApprovalRequest {
  expected_version: number
  segment_version: number
  content_version: number
  channels: Channel[]
}

export interface WithdrawApprovalRequest {
  expected_version: number
}

// ---- Field limits (backend campaign.NewDraft; runes after NFC + trim) ----

export const LIMITS = {
  nameMin: 1,
  nameMax: 120,
  briefMin: 10,
  briefMax: 2000,
} as const

// ---- Send / cancel (CP16/CP17A; backend httpapi/approvals.go) ----

export interface SendRequest {
  approval_id: string
  expected_version: number
}

/** POST /send: 202 started, or 200 already_started (idempotent repeat). */
export interface SendResponse {
  status: CampaignStatus
  approval_id: string
  released: Partial<Record<Channel, number>>
  already_started: boolean
  campaign: Campaign
  server_time: string
}

export interface CancelRequest {
  expected_version: number
}

export interface CancelResponse {
  status: 'cancelled'
  previous_status: CampaignStatus
  revoked_approval_id: string | null
  released_held_rows: number
  cancelled_deliveries: Partial<Record<Channel, number>>
  /** Rows in flight at the commit: each may still make at most one provider request. */
  in_flight: Partial<Record<Channel, number>>
  campaign: Campaign
  server_time: string
}

// ---- Delivery statistics (CP17B; backend delivery/stats.go, ARCHITECTURE §13) ----

export type DeliveryStatus =
  | 'pending'
  | 'in_flight'
  | 'retry_scheduled'
  | 'sent'
  | 'failed'
  | 'skipped_consent_revoked'
  | 'skipped_no_contact'
  | 'cancelled'

export type AttemptOutcome =
  | 'accepted'
  | 'replayed'
  | 'rate_limited'
  | 'transient_error'
  | 'timeout'
  | 'permanent_error'
  | 'unknown'

/** Exclusive partition of delivery rows; uncertain is an overlay, never added. */
export interface DeliveryPartition {
  approved_total: number
  pending: number
  in_flight: number
  retry_scheduled: number
  sent: number
  failed: number
  skipped_consent_revoked: number
  skipped_no_contact: number
  cancelled: number
  uncertain: number
}

/** Provider attempts (delivery_attempts rows), not deliveries. */
export interface DeliveryAttempts {
  total: number
  by_outcome: Record<AttemptOutcome, number>
  retries: number
  retried_deliveries: number
  recent: number
  per_second: number
}

export interface DeliveryReason {
  status: DeliveryStatus
  /** Sanitized code ([a-z0-9_]): e.g. no_consent, destination_rejected, campaign_cancelled, http_500. */
  code: string
  count: number
}

export interface ChannelDeliveryStats extends DeliveryPartition {
  attempts: DeliveryAttempts
  reasons: DeliveryReason[]
}

export type WorkerHealthStatus = 'fresh' | 'stale' | 'absent' | 'unknown'

export interface WorkerHealth {
  status: WorkerHealthStatus
  last_seen_at: string | null
  heartbeat_age_seconds: number | null
  stale_after_seconds: number
  fresh_processes: number
}

export type DeliveryOutcome =
  | 'not_started'
  | 'sending'
  | 'settling'
  | 'completed'
  | 'completed_with_failures'
  | 'cancelled'

export interface DeliveryStats {
  campaign_id: string
  campaign_status: CampaignStatus
  campaign_version: number
  approval_id: string | null
  segment_version: number | null
  content_id: number | null
  content_version: number | null
  channels: Partial<Record<Channel, ChannelDeliveryStats>>
  totals: DeliveryPartition
  attempts: DeliveryAttempts
  processed: number
  remaining: number
  /** processed / approved_total in [0, 1]; 0 for an empty snapshot. */
  progress: number
  campaign_terminal: boolean
  /** campaign_terminal and no row pending / in flight / scheduled: final numbers. */
  terminal: boolean
  outcome: DeliveryOutcome
  send_started_at: string | null
  completed_at: string | null
  cancelled_at: string | null
  worker: WorkerHealth
  updated_at: string
}

/** One delivery row in safe form (no user id, contact, vars, body or key). */
export interface DeliveryItem {
  id: number
  channel: Channel
  status: DeliveryStatus
  attempts: number
  rate_limited_attempts: number
  outcome_uncertain: boolean
  reason: string | null
  destination_masked: string | null
  sent_at: string | null
  updated_at: string
}

export interface DeliveryPage {
  campaign_id: string
  approval_id: string | null
  items: DeliveryItem[]
  next_cursor: string | null
  limit: number
}

export interface DeliveryListQuery {
  status?: DeliveryStatus
  channel?: Channel
  uncertain?: boolean
  cursor?: string
  limit?: number
}

/** Terminal event of GET /events. */
export interface DeliveryEndEvent {
  campaign_id: string
  campaign_status: CampaignStatus
  campaign_version: number
  outcome: DeliveryOutcome
}

export type DeliveryStreamEvent = { type: 'stats'; data: DeliveryStats } | { type: 'end'; data: DeliveryEndEvent }
