import { describe, expect, it } from 'vitest'
import { runeCount, validateBrief, validateName } from '@/composables/validation'

describe('client validation mirrors backend limits', () => {
  it('counts code points after NFC + trim', () => {
    expect(runeCount('  abc  ')).toBe(3)
    expect(runeCount('İstanbul')).toBe(8) // NFC composes İ
    expect(runeCount('😀😀')).toBe(2)
  })
  it('name: 1–120, no control characters', () => {
    expect(validateName('   ')).toMatch(/Enter a campaign name/)
    expect(validateName('a')).toBeNull()
    expect(validateName('x'.repeat(120))).toBeNull()
    expect(validateName('x'.repeat(121))).toMatch(/at most 120/)
    expect(validateName('bad\nname')).toMatch(/control characters/)
  })
  it('brief: 10–2000, newlines and tabs allowed', () => {
    expect(validateBrief('')).toMatch(/Describe/)
    expect(validateBrief('too short')).toMatch(/at least 10/)
    expect(validateBrief('ten chars!')).toBeNull()
    expect(validateBrief('line one\n\tline two')).toBeNull()
    expect(validateBrief('ş'.repeat(2000))).toBeNull()
    expect(validateBrief('ş'.repeat(2001))).toMatch(/at most 2000/)
    expect(validateBrief('valid brief \u0007 bell')).toMatch(/control characters/)
  })
})
