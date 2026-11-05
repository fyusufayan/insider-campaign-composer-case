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
