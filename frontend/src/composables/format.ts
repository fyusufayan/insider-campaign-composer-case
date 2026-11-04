// Display formatting helpers (locale-stable for tests: en-US grouping).

const NUM = new Intl.NumberFormat('en-US')

export function formatCount(n: number): string {
  return NUM.format(n)
}

export function formatDateTime(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return new Intl.DateTimeFormat('en-GB', {
    day: 'numeric',
    month: 'short',
    year: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  }).format(d)
}

export function percent(part: number, whole: number): number {
  if (whole <= 0) return 0
  return (part / whole) * 100
}

export const CHANNEL_LABEL = { sms: 'SMS', email: 'Email' } as const

export const STATUS_LABEL: Record<string, string> = {
  draft: 'Draft',
  segmented: 'Audience ready',
  content_ready: 'Content ready',
  pending_approval: 'Pending approval',
  approved: 'Approved',
  sending: 'Sending',
  completed: 'Completed',
  completed_with_failures: 'Completed with failures',
  cancelled: 'Cancelled',
}
