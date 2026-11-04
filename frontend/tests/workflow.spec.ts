// Integration-level: the whole app (router, store, client, components)
// against the stateful fake HTTP backend: create → interpret → channels.
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { FakeBackend } from './helpers/fakeBackend'
import { CAMPAIGN_ID, EXAMPLE_BRIEF } from './helpers/fixtures'
import { mountApp, settle, text, until, type MountedApp } from './helpers/mountApp'

let fake: FakeBackend
let app: MountedApp | null = null

beforeEach(() => {
  fake = new FakeBackend()
  localStorage.clear()
})
afterEach(() => {
  app?.wrapper.unmount()
  app = null
})

describe('full workflow', () => {
  it('create → interpret → select channels → reload by URL', async () => {
    app = await mountApp('/')
    const { wrapper, router } = app

    // Demo provider is announced in the shell.
    await until(() => expect(text(wrapper, 'provider-badge')).toMatch(/Demo AI/))

    // 1. Create
    await wrapper.find('#campaign-name').setValue('İstanbul cart recovery')
    await wrapper.find('#campaign-brief').setValue(EXAMPLE_BRIEF)
    await wrapper.find('[data-testid="create-submit"]').trigger('click')
    await wrapper.find('[data-testid="brief-form"]').trigger('submit')
    await until(() => expect(router.currentRoute.value.path).toBe(`/campaigns/${CAMPAIGN_ID}`))
    expect(fake.callsTo('POST', /^\/api\/v1\/campaigns$/)).toHaveLength(1)

    // 2. Interpret
    await until(() => expect(wrapper.find('[data-testid="interpret-button"]').exists()).toBe(true))
    await wrapper.find('[data-testid="interpret-button"]').trigger('click')
    await until(() => expect(wrapper.find('[data-testid="audience-stats"]').exists()).toBe(true))
    expect(text(wrapper, 'brief-text')).toBe(EXAMPLE_BRIEF) // what was asked
    expect(text(wrapper, 'interpretation-summary')).toMatch(/Users in Istanbul/) // what the AI understood
    expect(wrapper.findAll('[data-testid="rule-condition"]').map((c) => c.text())).toEqual([
      expect.stringContaining('City is İstanbul'),
      expect.stringContaining('Abandoned cart in the last 30 days'),
    ]) // what is applied
    expect(text(wrapper, 'kpi-matched')).toMatch(/412/) // who matches
    expect(text(wrapper, 'kpi-total')).toMatch(/569/) // how many messages

    // 3. Channels: email only, then both again
    const sms = wrapper.find('[data-testid="channel-sms"]')
    await sms.setValue(false)
    await until(() => expect(text(wrapper, 'kpi-total')).toMatch(/318/))
    await wrapper.find('[data-testid="channel-sms"]').setValue(true)
    await until(() => expect(text(wrapper, 'kpi-total')).toMatch(/569/))
    const puts = fake.callsTo('PUT', /\/channels$/).map((c) => c.body)
    expect(puts).toEqual([
      { channels: ['email'], expected_version: 2 },
      { channels: ['sms', 'email'], expected_version: 3 },
    ])
    expect(fake.campaigns.get(CAMPAIGN_ID)!.version).toBe(4)

    // 4. Restore through the URL (as after a browser refresh)
    app.wrapper.unmount()
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    await until(() => expect(text(app!.wrapper, 'kpi-total')).toMatch(/569/))
    expect(text(app.wrapper, 'campaign-name')).toBe('İstanbul cart recovery')
    expect(app.wrapper.text()).toMatch(/Version 4/)
    await settle()
  })

  it('renders untrusted campaign text (name, brief, AI notes) as plain text', async () => {
    const evil = '<img src=x onerror="window.__xss=1"><b>bold</b>'
    app = await mountApp('/')
    const { wrapper, router } = app
    await wrapper.find('#campaign-name').setValue(evil)
    await wrapper.find('#campaign-brief').setValue(`Users in Istanbul ${evil}`)
    await wrapper.find('[data-testid="brief-form"]').trigger('submit')
    await until(() => expect(router.currentRoute.value.name).toBe('campaign'))
    fake.interpretResult = (c) => {
      const res = fake.defaultInterpret(c)
      const next = fake.campaigns.get(c.id)!
      next.interpretation = { ...next.interpretation!, summary: evil, assumptions: [evil], unsupported: [evil] }
      return new Response(JSON.stringify({ campaign: next, audience: null, audience_error: { code: 'audience_unavailable', message: evil, details: [], retryable: true } }), {
        status: res.status,
        headers: { 'Content-Type': 'application/json' },
      })
    }
    await until(() => expect(wrapper.find('[data-testid="interpret-button"]').exists()).toBe(true))
    await wrapper.find('[data-testid="interpret-button"]').trigger('click')
    await until(() => expect(wrapper.find('[data-testid="interpretation-card"]').exists()).toBe(true))
    expect(text(wrapper, 'campaign-name')).toBe(evil)
    expect(text(wrapper, 'interpretation-summary')).toBe(evil)
    expect(document.querySelector('img[src="x"]')).toBeNull()
    expect(document.querySelector('main b')).toBeNull()
    expect((window as unknown as { __xss?: number }).__xss).toBeUndefined()
  })
})
