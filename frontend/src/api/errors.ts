// Maps ApiErrors to user-facing guidance per operation. Titles and hints are
// client-authored; the backend's envelope message (fixed, display-safe text)
// is shown as the description.

import type { ApiError } from './client'

export type Operation =
  | 'load'
  | 'create'
  | 'interpret'
  | 'audience'
  | 'channels'
  | 'content'
  | 'generate'
  | 'saveContent'
  | 'validateContent'
  | 'prepareApproval'
  | 'approval'
  | 'confirmApproval'
  | 'withdrawApproval'
  | 'send'
  | 'cancel'
  | 'stats'
  | 'deliveries'

export interface ErrorPresentation {
  title: string
  message: string
  hint?: string
  /** Offer re-running the same request. */
  canRetry: boolean
  /** Offer reloading the latest campaign (409s). */
  canReload: boolean
  /** The brief itself is the problem: offer revising it (in a new campaign). */
  canReviseBrief: boolean
}

export function presentError(err: ApiError, op: Operation): ErrorPresentation {
  const base: ErrorPresentation = {
    title: 'Something went wrong',
    message: err.message,
    canRetry: err.retryable,
    canReload: false,
    canReviseBrief: false,
  }

  if (err.kind === 'network' && err.code === 'stream_interrupted') {
    return {
      ...base,
      title: 'Connection lost during generation',
      message: 'The connection to the server was interrupted before the result arrived.',
      hint: 'The latest saved state was reloaded: if the content was saved before the interruption, it is shown below; otherwise nothing changed.',
      canRetry: true,
    }
  }
  if (err.kind === 'network' && (op === 'send' || op === 'cancel')) {
    return {
      ...base,
      title: 'Cannot reach the server',
      message:
        op === 'send'
          ? 'The response to Start sending was lost. The request may or may not have started the send; the latest campaign state is being reloaded — never assume either outcome.'
          : 'The response to Cancel was lost. The cancellation may or may not have been applied; the latest campaign state is being reloaded.',
      canRetry: false,
      canReload: true,
    }
  }
  if (err.kind === 'network') {
    return { ...base, title: 'Cannot reach the server', canRetry: true }
  }
  if (err.kind === 'invalid_response') {
    return { ...base, title: 'Unexpected server response', canRetry: true }
  }

  if (op === 'generate') {
    const g = presentGenerationError(err, base)
    if (g) return g
  }
  if (op === 'prepareApproval' || op === 'approval' || op === 'confirmApproval' || op === 'withdrawApproval') {
    const a = presentApprovalError(err, op, base)
    if (a) return a
  }
  if (op === 'send' || op === 'cancel' || op === 'stats' || op === 'deliveries') {
    const d = presentDeliveryError(err, op, base)
    if (d) return d
  }

  switch (err.code) {
    case 'validation_failed':
      return { ...base, title: 'Please fix the highlighted fields', canRetry: false }
    case 'not_found':
      return {
        ...base,
        title: 'Campaign not found',
        message: 'No campaign exists with this ID. Check the link or create a new campaign.',
        canRetry: false,
      }
    case 'invalid_brief':
      return {
        ...base,
        title: 'The brief cannot be turned into a supported audience',
        hint: 'Nothing was saved. Revise the brief using supported targeting (city, country, gender, age, language, sign-up or activity recency, cart and purchase events).',
        canRetry: false,
        canReviseBrief: true,
      }
    case 'ai_output_invalid':
      return {
        ...base,
        title: 'The AI proposal failed validation',
        hint: 'The proposal was rejected after one automatic repair attempt and nothing was saved. You can try again or rephrase the brief.',
        canRetry: true,
        canReviseBrief: true,
      }
    case 'llm_refused':
      return {
        ...base,
        title: 'The AI declined to interpret this brief',
        hint: 'Nothing was saved. Rephrase the brief and try again.',
        canRetry: false,
        canReviseBrief: true,
      }
    case 'llm_unavailable':
      return { ...base, title: 'The AI service is unavailable', canRetry: true }
    case 'llm_truncated':
      return { ...base, title: 'The AI answer was incomplete', canRetry: true, canReviseBrief: true }
    case 'llm_rejected':
      return {
        ...base,
        title: 'The AI service rejected the request',
        hint: 'This usually indicates a server configuration problem.',
        canRetry: false,
      }
    case 'audience_unavailable':
      return { ...base, title: 'Audience statistics are temporarily unavailable', canRetry: true }
    case 'version_conflict':
      return {
        ...base,
        title: 'This campaign was changed elsewhere',
        message:
          op === 'interpret'
            ? 'The campaign changed while the AI was working, so its result was discarded. Reload the latest version before trying again.'
            : op === 'saveContent'
              ? 'Your edits were not saved because the campaign was modified elsewhere. Your edits are kept here: reload the latest version, review the differences and save again.'
              : 'Your change was not saved because the campaign was modified by another request. Reload the latest version, then re-apply your change.',
        canRetry: false,
        canReload: true,
      }
    case 'invalid_state':
      return { ...base, title: 'This action is not allowed right now', canRetry: false, canReload: true }
    case 'internal':
      return { ...base, title: 'Server error', canRetry: true }
    default:
      return base
  }
}

/** Generation-specific wording (the model writes content, not the audience). */
function presentGenerationError(err: ApiError, base: ErrorPresentation): ErrorPresentation | null {
  switch (err.code) {
    case 'llm_refused':
      return {
        ...base,
        title: 'The AI declined to write this content',
        hint: 'Nothing was saved. Write the messages manually below, or revise the brief in a new campaign.',
        canRetry: false,
        canReviseBrief: true,
      }
    case 'ai_output_invalid':
      return {
        ...base,
        title: 'The generated content failed validation',
        hint: 'The draft was rejected after one automatic repair attempt and nothing was saved. Try again, or write the messages manually.',
        canRetry: true,
      }
    case 'llm_truncated':
      return { ...base, title: 'The AI answer was incomplete', hint: 'Nothing was saved.', canRetry: true }
    case 'llm_unavailable':
      return { ...base, title: 'The AI service is unavailable', hint: 'Nothing was saved.', canRetry: true }
    case 'version_conflict':
      return {
        ...base,
        title: 'The campaign changed during generation',
        message: 'The campaign was modified while the AI was writing, so the generated content was discarded and nothing was saved.',
        canRetry: false,
        canReload: true,
      }
    default:
      return null
  }
}

/** Approval-specific wording (CP15). Nothing is retried automatically. */
function presentApprovalError(err: ApiError, op: Operation, base: ErrorPresentation): ErrorPresentation | null {
  switch (err.code) {
    case 'approval_expired':
      return {
        ...base,
        title: 'The approval has expired',
        message:
          'Its time limit passed, so it can no longer be confirmed and its recipient snapshot was released. Nothing was sent.',
        hint: 'Reload the campaign and prepare a new approval to review a fresh snapshot.',
        canRetry: false,
        canReload: true,
      }
    case 'approval_mismatch':
      return {
        ...base,
        title: 'The approval no longer matches what you reviewed',
        hint: 'Nothing was confirmed. Reload the campaign and review the approval again.',
        canRetry: false,
        canReload: true,
      }
    case 'empty_audience':
      return {
        ...base,
        title: 'Nobody can be messaged',
        hint: 'No matched user has both a contact and consent for the selected channels. Nothing was prepared.',
        canRetry: false,
      }
    case 'audience_changed':
      return { ...base, title: 'Customer data changed during preparation', canRetry: true }
    case 'campaign_busy':
      return { ...base, title: 'The campaign is busy', canRetry: true }
    case 'not_found':
      return op === 'prepareApproval'
        ? null
        : {
            ...base,
            title: 'Approval not found',
            message: 'This approval does not exist for this campaign. Reload the campaign.',
            canRetry: false,
            canReload: true,
          }
    case 'version_conflict':
      return {
        ...base,
        title: 'This campaign was changed elsewhere',
        message:
          op === 'confirmApproval'
            ? 'Nothing was confirmed: the campaign changed since you opened this review (another tab, an expiry or a withdrawal). Reload and review the current state.'
            : op === 'withdrawApproval'
              ? 'Nothing was withdrawn: the campaign changed since you loaded it. Reload and check the current approval.'
              : 'Nothing was prepared: the campaign changed since you loaded it. Reload the latest version first.',
        canRetry: false,
        canReload: true,
      }
    case 'invalid_state':
      return { ...base, title: 'This approval action is not allowed right now', canRetry: false, canReload: true }
    default:
      return null
  }
}

/** Send / cancel / statistics wording (CP17B). Nothing is retried automatically. */
function presentDeliveryError(err: ApiError, op: Operation, base: ErrorPresentation): ErrorPresentation | null {
  switch (err.code) {
    case 'approval_expired':
      return {
        ...base,
        title: 'The approval has expired',
        message: 'Nothing was sent: the approval passed its time limit and its recipient snapshot was released.',
        hint: 'Reload the campaign and prepare a new approval.',
        canRetry: false,
        canReload: true,
      }
    case 'approval_mismatch':
      return {
        ...base,
        title: 'The approval no longer matches the campaign',
        hint: 'Nothing was sent. Reload the campaign and review it again.',
        canRetry: false,
        canReload: true,
      }
    case 'version_conflict':
      return {
        ...base,
        title: 'This campaign was changed elsewhere',
        message:
          op === 'send'
            ? 'Nothing was started by this request: the campaign changed since you loaded it. Reload and check its current state.'
            : 'Nothing was cancelled by this request: the campaign changed since you loaded it (for example, it just finished). Reload and check its current state.',
        canRetry: false,
        canReload: true,
      }
    case 'invalid_state':
      return {
        ...base,
        title: op === 'send' ? 'Sending cannot start' : 'The campaign cannot be cancelled',
        hint: 'The latest campaign state is being reloaded.',
        canRetry: false,
        canReload: true,
      }
    case 'campaign_busy':
      return { ...base, title: 'The campaign is busy', canRetry: false, canReload: true }
    case 'too_many_streams':
      return { ...base, title: 'Too many live views are open', canRetry: true }
    case 'not_found':
      return op === 'send'
        ? { ...base, title: 'Approval not found', hint: 'Reload the campaign.', canRetry: false, canReload: true }
        : null
    default:
      return null
  }
}
