import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { FakeBackend, deferred, json } from './helpers/fakeBackend'
import { CAMPAIGN_ID, EXAMPLE_BRIEF, draftCampaign, errorEnvelope } from './helpers/fixtures'
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

describe('campaign creation', () => {
  it('creates a campaign and navigates to its stable URL', async () => {
    app = await mountApp('/')
    const { wrapper, router } = app
    expect(router.currentRoute.value.name).toBe('campaign-new')

    await wrapper.find('#campaign-name').setValue('  İstanbul cart recovery ')
    await wrapper.find('#campaign-brief').setValue(EXAMPLE_BRIEF)
    await wrapper.find('[data-testid="brief-form"]').trigger('submit')
    await settle()

    const posts = fake.callsTo('POST', /^\/api\/v1\/campaigns$/)
    expect(posts).toHaveLength(1)
    expect(posts[0]!.body).toEqual({ name: '  İstanbul cart recovery ', brief: EXAMPLE_BRIEF })

    await until(() => expect(router.currentRoute.value.fullPath).toBe(`/campaigns/${CAMPAIGN_ID}`))
    await until(() => expect(text(wrapper, 'campaign-name')).toBe('İstanbul cart recovery'))
    expect(text(wrapper, 'brief-text')).toBe(EXAMPLE_BRIEF)
    expect(text(wrapper, 'status-badge')).toBe('Draft')
    await until(() => expect(document.title).toBe('İstanbul cart recovery · Campaign Composer'))
    // The created campaign was adopted: no extra GET was needed.
    expect(fake.callsTo('GET', /\/api\/v1\/campaigns\//)).toHaveLength(0)
    // Draft: no audience request.
    expect(fake.callsTo('GET', /\/audience$/)).toHaveLength(0)
  })

  it('validates on submit, focuses the first invalid field and sends nothing', async () => {
    app = await mountApp('/campaigns/new')
    const { wrapper } = app
    await wrapper.find('[data-testid="brief-form"]').trigger('submit')
    await settle(1)
    expect(text(wrapper, 'name-error')).toMatch(/Enter a campaign name/)
    expect(text(wrapper, 'brief-error')).toMatch(/Describe the campaign/)
    expect(document.activeElement?.id).toBe('campaign-name')
    expect(wrapper.find('#campaign-name').attributes('aria-invalid')).toBe('true')
    expect(wrapper.find('#campaign-name').attributes('aria-describedby')).toBe('campaign-name-error')

    await wrapper.find('#campaign-name').setValue('Name')
    await wrapper.find('#campaign-brief').setValue('too short')
    await wrapper.find('[data-testid="brief-form"]').trigger('submit')
    await settle(1)
    expect(wrapper.find('[data-testid="name-error"]').exists()).toBe(false)
    expect(text(wrapper, 'brief-error')).toMatch(/at least 10 characters \(currently 9\)/)
    expect(document.activeElement?.id).toBe('campaign-brief')
    expect(fake.callsTo('POST', /campaigns$/)).toHaveLength(0)
  })

  it('shows a field error only after the field was left (blur)', async () => {
    app = await mountApp('/campaigns/new')
    const { wrapper } = app
    await wrapper.find('#campaign-brief').setValue('short')
    expect(wrapper.find('[data-testid="brief-error"]').exists()).toBe(false)
    await wrapper.find('#campaign-brief').trigger('blur')
    expect(text(wrapper, 'brief-error')).toMatch(/at least 10/)
  })

  it('submits with Ctrl/⌘+Enter from the brief', async () => {
    app = await mountApp('/campaigns/new')
    const { wrapper } = app
    await wrapper.find('#campaign-name').setValue('Keyboard')
    await wrapper.find('#campaign-brief').setValue(EXAMPLE_BRIEF)
    await wrapper.find('#campaign-brief').trigger('keydown', { key: 'Enter', ctrlKey: true })
    await settle()
    expect(fake.callsTo('POST', /campaigns$/)).toHaveLength(1)
  })

  it('prevents duplicate submissions while pending', async () => {
    const gate = deferred<Response>()
    fake.override('POST', /^\/api\/v1\/campaigns$/, () => gate.promise)
    app = await mountApp('/campaigns/new')
    const { wrapper, router } = app
    await wrapper.find('#campaign-name').setValue('Once')
    await wrapper.find('#campaign-brief').setValue(EXAMPLE_BRIEF)
    await wrapper.find('[data-testid="brief-form"]').trigger('submit')
    await settle(1)
    expect(wrapper.find('[data-testid="create-submit"]').attributes('disabled')).toBeDefined()
    expect(text(wrapper, 'create-submit')).toBe('Creating…')
    await wrapper.find('[data-testid="brief-form"]').trigger('submit')
    await wrapper.find('#campaign-brief').trigger('keydown', { key: 'Enter', metaKey: true })
    expect(fake.callsTo('POST', /campaigns$/)).toHaveLength(1)

    gate.resolve(json(201, draftCampaign({ name: 'Once' })))
    await until(() => expect(router.currentRoute.value.name).toBe('campaign'))
  })

  it('maps server validation details to the fields', async () => {
    fake.override('POST', /^\/api\/v1\/campaigns$/, () =>
      json(
        422,
        errorEnvelope('validation_failed', 'request is invalid', {
          details: [{ path: 'brief', code: 'invalid_value', message: 'must not contain control characters' }],
        }),
      ),
    )
    app = await mountApp('/campaigns/new')
    const { wrapper, router } = app
    await wrapper.find('#campaign-name').setValue('Server says no')
    await wrapper.find('#campaign-brief').setValue(EXAMPLE_BRIEF)
    await wrapper.find('[data-testid="brief-form"]').trigger('submit')
    await until(() => expect(text(wrapper, 'brief-error')).toBe('must not contain control characters'))
    expect(router.currentRoute.value.name).toBe('campaign-new')
    // Editing the field clears the server error.
    await wrapper.find('#campaign-brief').setValue(EXAMPLE_BRIEF + ' ')
    expect(wrapper.find('[data-testid="brief-error"]').exists()).toBe(false)
  })

  it('shows a network error without leaving the form', async () => {
    fake.override('POST', /^\/api\/v1\/campaigns$/, () => {
      throw new TypeError('Failed to fetch')
    })
    app = await mountApp('/campaigns/new')
    const { wrapper } = app
    await wrapper.find('#campaign-name').setValue('Offline')
    await wrapper.find('#campaign-brief').setValue(EXAMPLE_BRIEF)
    await wrapper.find('[data-testid="brief-form"]').trigger('submit')
    await until(() => expect(text(wrapper, 'error-notice')).toMatch(/Cannot reach the server/))
    expect((wrapper.find('#campaign-brief').element as HTMLTextAreaElement).value).toBe(EXAMPLE_BRIEF)
  })
})
