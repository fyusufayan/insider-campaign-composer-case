// Client-side checks mirroring campaign.NewDraft (UX only: the server is
// authoritative and its 422 details are always displayed). Lengths are
// counted in Unicode code points after NFC normalization and trimming, the
// same way the backend counts runes.

import { LIMITS } from '@/api/types'

export function normalizeText(s: string): string {
  return s.normalize('NFC').trim()
}

export function runeCount(s: string): number {
  let n = 0
  for (const _ of normalizeText(s)) n++
  return n
}

// unicode.IsControl: C0 controls, DEL and C1 controls.
const CONTROL = /[\u0000-\u001f\u007f-\u009f]/
const CONTROL_EXCEPT_NEWLINE_TAB = /[\u0000-\u0008\u000b-\u001f\u007f-\u009f]/

export function validateName(name: string): string | null {
  const n = runeCount(name)
  if (n === 0) return 'Enter a campaign name.'
  if (n > LIMITS.nameMax) return `Use at most ${LIMITS.nameMax} characters (currently ${n}).`
  if (CONTROL.test(normalizeText(name))) return 'The name must not contain control characters.'
  return null
}

export function validateBrief(brief: string): string | null {
  const n = runeCount(brief)
  if (n === 0) return 'Describe the campaign in a few sentences.'
  if (n < LIMITS.briefMin) return `Write at least ${LIMITS.briefMin} characters (currently ${n}).`
  if (n > LIMITS.briefMax) return `Use at most ${LIMITS.briefMax} characters (currently ${n}).`
  if (CONTROL_EXCEPT_NEWLINE_TAB.test(normalizeText(brief))) return 'The brief must not contain control characters.'
  return null
}
