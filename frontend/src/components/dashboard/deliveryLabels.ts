// Display labels for delivery statuses, reason codes and connection states.
// Unknown codes are shown verbatim (they are sanitized [a-z0-9_] codes,
// rendered as text, never as HTML).
import type { ConnectionState } from '@/composables/useCampaignEvents'
import type { DeliveryStatus, WorkerHealthStatus } from '@/api/types'

export const DELIVERY_STATUS_LABEL: Record<DeliveryStatus, string> = {
  pending: 'Queued',
  in_flight: 'In flight',
  retry_scheduled: 'Retry scheduled',
  sent: 'Sent (accepted)',
  failed: 'Failed',
  skipped_consent_revoked: 'Skipped — consent revoked',
  skipped_no_contact: 'Skipped — no valid contact',
  cancelled: 'Cancelled',
}

const REASONS: Record<string, string> = {
  no_consent: 'Consent no longer opted in at delivery time',
  no_contact: 'No valid contact at delivery time',
  campaign_cancelled: 'Campaign was cancelled',
  campaign_not_sending: 'Campaign stopped sending',
  destination_rejected: 'Provider rejected the destination',
  invalid_request: 'Provider rejected the request',
  idempotency_key_reuse: 'Provider rejected a reused key',
  rate_limited: 'Provider rate limit (429)',
  timeout: 'Provider timed out',
  connection_failed: 'Could not connect to the provider',
  connection_error: 'Connection broke during the request',
  unreadable_response: 'Unreadable provider response',
  lease_expired: 'Worker stopped mid-attempt (recovered)',
  aborted: 'Request aborted',
  content_invalid: 'Approved content no longer renders',
  invalid_vars: 'Frozen personalization values are invalid',
  unspecified: 'No reason recorded',
  other: 'Other reasons',
}

export function reasonLabel(code: string): string {
  if (REASONS[code]) return REASONS[code]
  const m = /^http_(\d{3})$/.exec(code)
  if (m) return `Provider answered HTTP ${m[1]}`
  return code
}

export const CONNECTION_LABEL: Record<ConnectionState, string> = {
  idle: 'Not connected',
  connecting: 'Connecting…',
  live: 'Live',
  reconnecting: 'Reconnecting…',
  ended: 'Final',
  polling: 'Refreshing periodically',
}

export const WORKER_LABEL: Record<WorkerHealthStatus, string> = {
  fresh: 'Worker online',
  stale: 'Worker heartbeat stale',
  absent: 'No worker heartbeat',
  unknown: 'Worker status unknown',
}
