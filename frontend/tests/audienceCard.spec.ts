import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import AudienceCard from '@/components/composer/AudienceCard.vue'
import type { AudienceStats, Channel } from '@/api/types'
import { audienceFor } from './helpers/fixtures'
import { spacedText } from './helpers/mountApp'

function mountCard(stats: AudienceStats | null, extra: Record<string, unknown> = {}) {
  const channels = (stats ? Object.keys(stats.by_channel) : []) as Channel[]
  return mount(AudienceCard, {
    props: {
      stats,
      current: true,
      loading: false,
      error: null,
      errorAfterInterpret: false,
      selectedChannels: channels,
      savedChannels: channels,
      canEditChannels: true,
      busy: false,
      savingChannels: false,
      channelsError: null,
      conflict: false,
      unsavedChannels: null,
      ...extra,
    },
  })
}

describe('AudienceCard', () => {
  it('distinguishes users from messages and shows both identities', () => {
    const w = mountCard(audienceFor(['sms', 'email']))
    expect(spacedText(w.find('[data-testid="kpi-matched"]').element)).toBe('Matched users 412 people matching the rule')
    expect(spacedText(w.find('[data-testid="kpi-total"]').element)).toBe(
      'Total eligible messages 569 messages across selected channels',
    )
    expect(w.find('[data-testid="sms-eligible"]').text()).toBe('251')
    expect(w.find('[data-testid="sms-no_consent"]').text()).toBe('140')
    expect(w.find('[data-testid="sms-no_contact"]').text()).toBe('21')
    expect(w.find('[data-testid="email-eligible"]').text()).toBe('318')
    expect(w.find('[data-testid="reach-both"]').text()).toBe('202')
    expect(w.find('[data-testid="reach-unreachable"]').text()).toBe('45')
    expect(w.find('[data-testid="identity-violation"]').exists()).toBe(false)
  })

  it('formats large counts with grouping', () => {
    const s = audienceFor(['sms'])
    const big: AudienceStats = {
      ...s,
      matched_users: 12000,
      by_channel: { sms: { eligible: 10000, no_consent: 1500, no_contact: 500 } },
      reach_users: { both: 0, sms_only: 10000, email_only: 0, unreachable: 2000 },
      messages: 10000,
    }
    const w = mountCard(big)
    expect(spacedText(w.find('[data-testid="kpi-matched"]').element)).toMatch(/12,000/)
  })

  it('flags statistics that violate the partition identities', () => {
    const s = audienceFor(['sms'])
    const w = mountCard({ ...s, reach_users: { ...s.reach_users, unreachable: s.reach_users.unreachable + 1 } })
    expect(w.find('[data-testid="identity-violation"]').exists()).toBe(true)
  })

  it('empty selection: zero messages, matched users preserved, explanation shown', () => {
    const w = mountCard(audienceFor([]))
    expect(w.find('[data-testid="no-channels"]').exists()).toBe(true)
    expect(spacedText(w.find('[data-testid="kpi-total"]').element)).toMatch(/^Total eligible messages 0/)
    expect(spacedText(w.find('[data-testid="kpi-matched"]').element)).toMatch(/412/)
    expect(w.find('[data-testid="partition-sms"]').exists()).toBe(false)
  })

  it('dims numbers from a previous selection while refreshing', () => {
    const w = mountCard(audienceFor(['sms', 'email']), { current: false, loading: true })
    expect(w.find('[data-testid="audience-stale"]').text()).toMatch(/Updating for the current selection/)
    expect(w.find('[data-testid="audience-stats"]').classes()).toContain('stale')
  })

  it('emits the next channel list on toggle, including []', async () => {
    const w = mountCard(audienceFor(['sms']))
    await w.find('[data-testid="channel-sms"]').setValue(false)
    expect(w.emitted('change-channels')).toEqual([[[]]])
  })

  it('locks channels when editing is not allowed', () => {
    const w = mountCard(audienceFor(['sms']), { canEditChannels: false })
    expect(w.find('[data-testid="channel-selector"]').attributes('disabled')).toBeDefined()
  })
})
