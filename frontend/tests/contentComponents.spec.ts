import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import EmailPreview from '@/components/composer/EmailPreview.vue'
import LengthCounter from '@/components/composer/LengthCounter.vue'
import SmsPreview from '@/components/composer/SmsPreview.vue'

const EVIL = '<img src=x onerror="window.__xss=1"><script>alert(1)</script> & "q"'

describe('previews render untrusted text as text', () => {
  it('SmsPreview', () => {
    const w = mount(SmsPreview, { props: { text: EVIL, unavailable: '' } })
    expect(w.find('[data-testid="sms-preview-text"]').text()).toBe(EVIL)
    expect(w.find('img').exists()).toBe(false)
    expect(w.find('script').exists()).toBe(false)
  })

  it('EmailPreview builds a fully sandboxed, escaped document', () => {
    const w = mount(EmailPreview, {
      props: { email: { subject: EVIL, body: `line1\n${EVIL}`, footer: 'Footer' }, unavailable: '' },
    })
    const frame = w.find('iframe')
    expect(frame.attributes('sandbox')).toBe('')
    const doc = frame.attributes('srcdoc')!
    expect(doc).not.toContain('<img')
    expect(doc).not.toContain('<script>alert')
    expect(doc).toContain('&lt;img src=x onerror=&quot;window.__xss=1&quot;&gt;')
    expect(doc).toContain('&amp; &quot;q&quot;')
    expect(doc).toContain('Footer')
    expect(w.find('[data-testid="email-preview-subject"]').text()).toContain(EVIL)
  })

  it('shows why a preview is unavailable', () => {
    const w = mount(EmailPreview, { props: { email: null, unavailable: 'Fix the errors above to see the preview.' } })
    expect(w.find('iframe').exists()).toBe(false)
    expect(w.text()).toContain('Fix the errors above')
  })
})

describe('LengthCounter', () => {
  it.each([
    [{ chars_worst_case: 100, limit: 160 }, 'ok'],
    [{ chars_worst_case: 150, limit: 160 }, 'near'],
    [{ chars_worst_case: 161, limit: 160 }, 'over'],
    [null, 'unknown'],
  ])('%j → %s', (stats, state) => {
    const w = mount(LengthCounter, { props: { stats, limit: 160, typed: ' şğü ', checking: false, testid: 'c' } })
    expect(w.attributes('data-state')).toBe(state)
    expect(w.text()).toContain('3 characters typed')
    expect(w.find('[data-testid="c-worst"]').text()).toBe(stats ? `${stats.chars_worst_case} / 160` : '— / 160')
  })
})
