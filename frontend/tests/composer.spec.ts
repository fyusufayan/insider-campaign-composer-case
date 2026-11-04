import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { FakeBackend, deferred, json } from './helpers/fakeBackend'
import {
  CAMPAIGN_ID,
  EXAMPLE_BRIEF,
  OTHER_ID,
  audienceFor,
  draftCampaign,
  errorEnvelope,
  segmentedCampaign,
} from './helpers/fixtures'
import { mountApp, settle, spacedText, text, until, type MountedApp } from './helpers/mountApp'

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

function checkbox(ch: 'sms' | 'email') {
  return app!.wrapper.find(`[data-testid="channel-${ch}"]`)
}
function isChecked(ch: 'sms' | 'email'): boolean {
  return (checkbox(ch).element as HTMLInputElement).checked
}

describe('loading a saved campaign by URL', () => {
  it('loads a segmented campaign and its live audience on direct navigation', async () => {
    fake.seed(segmentedCampaign())
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    const { wrapper } = app
    await until(() => expect(text(wrapper, 'kpi-matched')).toMatch(/412/))
    expect(fake.callsTo('GET', new RegExp(`/campaigns/${CAMPAIGN_ID}$`))).toHaveLength(1)
    expect(fake.callsTo('GET', /\/audience$/)).toHaveLength(1)
    expect(text(wrapper, 'campaign-name')).toBe('İstanbul cart recovery')
    expect(text(wrapper, 'campaign-id')).toBe(CAMPAIGN_ID)
    expect(text(wrapper, 'status-badge')).toBe('Audience ready')
    expect(text(wrapper, 'interpretation-summary')).toMatch(/^Users in Istanbul who added a product/)
    expect(isChecked('sms')).toBe(true)
    expect(isChecked('email')).toBe(true)
    // Remembered locally for the "recently opened" list.
    expect(localStorage.getItem('composer.recentCampaigns.v1')).toContain(CAMPAIGN_ID)
  })

  it('loads a draft without requesting the audience', async () => {
    fake.seed(draftCampaign())
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    const { wrapper } = app
    await until(() => expect(text(wrapper, 'brief-text')).toBe(EXAMPLE_BRIEF))
    expect(wrapper.find('[data-testid="interpret-button"]').text()).toBe('Interpret brief')
    expect(wrapper.find('[data-testid="audience-placeholder"]').exists()).toBe(true)
    expect(fake.callsTo('GET', /\/audience$/)).toHaveLength(0)
  })

  it('shows not found for an unknown campaign', async () => {
    app = await mountApp(`/campaigns/${OTHER_ID}`)
    await until(() => expect(text(app!.wrapper, 'load-error')).toMatch(/Campaign not found/))
  })

  it('rejects a malformed id without calling the API', async () => {
    app = await mountApp('/campaigns/not-a-uuid')
    expect(app.wrapper.find('[data-testid="invalid-id"]').exists()).toBe(true)
    expect(fake.calls.filter((c) => c.path.startsWith('/api/'))).toHaveLength(0)
  })
})

describe('interpretation', () => {
  it('renders summary, active conditions, model identity, content brief and real-shaped statistics', async () => {
    fake.seed(draftCampaign())
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    const { wrapper } = app
    await until(() => expect(wrapper.find('[data-testid="interpret-button"]').exists()).toBe(true))
    await wrapper.find('[data-testid="interpret-button"]').trigger('click')
    await until(() => expect(wrapper.find('[data-testid="interpretation-card"]').exists()).toBe(true))

    const posts = fake.callsTo('POST', /\/interpret$/)
    expect(posts).toHaveLength(1)
    expect(posts[0]!.body).toEqual({})

    // What the AI understood
    expect(text(wrapper, 'interpretation-summary')).toBe(
      'Users in Istanbul who added a product to their cart in the last 30 days and have not purchased since.',
    )
    expect(text(wrapper, 'assumptions')).toMatch(/no purchase after the cart addition/)
    expect(text(wrapper, 'interpretation-card')).toMatch(/Nothing — every request in the brief was expressible/)
    // Demo is never presented as a live model
    expect(text(wrapper, 'ai-source')).toMatch(/Demo interpreter · not a live model/)
    expect(text(wrapper, 'ai-source')).toMatch(/Prompt segment_v1/)
    expect(text(wrapper, 'content-brief')).toMatch(/cart recovery/)
    expect(text(wrapper, 'offer')).toBe('30% discount')

    // Active targeting (from the normalized rule)
    const conds = wrapper.findAll('[data-testid="rule-condition"]').map((c) => c.text())
    expect(conds[0]).toMatch(/City is İstanbul/)
    expect(conds[1]).toMatch(/Abandoned cart in the last 30 days/)
    expect(text(wrapper, 'rule-match')).toMatch(/ALL Users must match all of the following/)

    // Audience from the interpret response itself (no extra GET)
    expect(fake.callsTo('GET', /\/audience$/)).toHaveLength(0)
    expect(text(wrapper, 'kpi-matched')).toMatch(/412/)
    expect(text(wrapper, 'kpi-sms')).toMatch(/251 messages/)
    expect(text(wrapper, 'kpi-email')).toMatch(/318 messages/)
    expect(text(wrapper, 'kpi-total')).toMatch(/569/)
    expect(text(wrapper, 'sms-identity')).toBe('251 eligible + 140 no consent + 21 no contact = 412 matched users')
    expect(text(wrapper, 'email-identity')).toBe('318 eligible + 82 no consent + 12 no contact = 412 matched users')
    expect(text(wrapper, 'reach-identity')).toBe(
      '202 both + 49 SMS only + 116 email only + 45 unreachable = 412 matched users',
    )
    expect(wrapper.find('[data-testid="identity-violation"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="computed-at"]').attributes('datetime')).toBe('2026-11-04T10:01:02Z')
    expect(text(wrapper, 'status-badge')).toBe('Audience ready')
  })

  it('shows the initially suggested channels (sms only)', async () => {
    fake.seed(draftCampaign())
    fake.interpretResult = (c) => fake.defaultInterpret(c, { suggested: ['sms'] })
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    await until(() => expect(app!.wrapper.find('[data-testid="interpret-button"]').exists()).toBe(true))
    await app.wrapper.find('[data-testid="interpret-button"]').trigger('click')
    await until(() => expect(app!.wrapper.find('[data-testid="audience-stats"]').exists()).toBe(true))
    expect(isChecked('sms')).toBe(true)
    expect(isChecked('email')).toBe(false)
    expect(text(app.wrapper, 'kpi-email')).toMatch(/0 channel not selected/)
    expect(app.wrapper.find('[data-testid="partition-email"]').exists()).toBe(false)
    expect(text(app.wrapper, 'reach-identity')).toBe(
      '0 both + 251 SMS only + 0 email only + 161 unreachable = 412 matched users',
    )
  })

  it('shows progress and disables controls while interpreting', async () => {
    fake.seed(segmentedCampaign())
    const gate = deferred<Response>()
    fake.override('POST', /\/interpret$/, () => gate.promise)
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    const { wrapper } = app
    await until(() => expect(wrapper.find('[data-testid="audience-stats"]').exists()).toBe(true))
    await wrapper.find('[data-testid="interpret-button"]').trigger('click')
    await settle(1)
    expect(text(wrapper, 'interpret-button')).toBe('Interpreting…')
    expect(wrapper.find('[data-testid="interpret-progress"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="channel-selector"]').attributes('disabled')).toBeDefined()
    // A second click does not start a second request.
    await wrapper.find('[data-testid="interpret-button"]').trigger('click')
    expect(fake.callsTo('POST', /\/interpret$/)).toHaveLength(1)
    gate.resolve(fake.defaultInterpret(fake.campaigns.get(CAMPAIGN_ID)!))
    await until(() => expect(text(wrapper, 'interpret-button')).toBe('Re-interpret brief'))
  })
})

describe('channel selection', () => {
  it('deselecting all channels sends [], then shows zero messages with matched users preserved', async () => {
    fake.seed(segmentedCampaign())
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    const { wrapper } = app
    await until(() => expect(text(wrapper, 'kpi-total')).toMatch(/569/))

    await checkbox('sms').setValue(false)
    await until(() => expect(fake.callsTo('PUT', /\/channels$/)).toHaveLength(1))
    expect(fake.callsTo('PUT', /\/channels$/)[0]!.body).toEqual({ channels: ['email'], expected_version: 2 })
    await until(() => expect(text(wrapper, 'kpi-total')).toMatch(/318/))

    await checkbox('email').setValue(false)
    await until(() => expect(fake.callsTo('PUT', /\/channels$/)).toHaveLength(2))
    // Uses the version returned by the previous successful update.
    expect(fake.callsTo('PUT', /\/channels$/)[1]!.body).toEqual({ channels: [], expected_version: 3 })

    await until(() => expect(wrapper.find('[data-testid="no-channels"]').exists()).toBe(true))
    expect(fake.callsTo('GET', /\/audience$/)).toHaveLength(3) // initial + after each update
    expect(text(wrapper, 'kpi-total')).toMatch(/^Total eligible messages 0/)
    expect(text(wrapper, 'kpi-sms')).toMatch(/0 channel not selected/)
    expect(text(wrapper, 'kpi-matched')).toMatch(/412/)
    expect(text(wrapper, 'no-channels')).toMatch(/0 messages\). The rule still matches 412 users/)
    expect(text(wrapper, 'reach-identity')).toBe('0 both + 0 SMS only + 0 email only + 412 unreachable = 412 matched users')
    expect(wrapper.find('[data-testid="partition-sms"]').exists()).toBe(false)
    expect(isChecked('sms')).toBe(false)
    expect(isChecked('email')).toBe(false)
  })

  it('prevents a second channel mutation while one is pending', async () => {
    fake.seed(segmentedCampaign())
    const gate = deferred<Response>()
    fake.override('PUT', /\/channels$/, () => gate.promise)
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    const { wrapper } = app
    await until(() => expect(wrapper.find('[data-testid="audience-stats"]').exists()).toBe(true))
    await checkbox('sms').setValue(false)
    await settle(1)
    expect(wrapper.find('[data-testid="channel-selector"]').attributes('disabled')).toBeDefined()
    expect(wrapper.find('[data-testid="interpret-button"]').attributes('disabled')).toBeDefined()
    expect(text(wrapper, 'channel-selector')).toMatch(/Saving…/)
    expect(fake.callsTo('PUT', /\/channels$/)).toHaveLength(1)
    gate.resolve(json(200, { ...segmentedCampaign(), channels: ['email'], version: 3 }))
    await until(() => expect(wrapper.find('[data-testid="channel-selector"]').attributes('disabled')).toBeUndefined())
  })

  it('409 version_conflict: nothing is retried; reload, then explicit re-apply', async () => {
    fake.seed(segmentedCampaign())
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    const { wrapper } = app
    await until(() => expect(wrapper.find('[data-testid="audience-stats"]').exists()).toBe(true))

    // Another tab removes email meanwhile (version 2 → 3).
    fake.externalChange(CAMPAIGN_ID, { channels: ['sms'] })

    // The user, still looking at version 2, unchecks email (wants SMS only).
    await checkbox('email').setValue(false)
    await until(() => expect(wrapper.find('[data-testid="channels-conflict"]').exists()).toBe(true))
    const puts = fake.callsTo('PUT', /\/channels$/)
    expect(puts).toHaveLength(1)
    expect(puts[0]!.body).toEqual({ channels: ['sms'], expected_version: 2 })
    expect(text(wrapper, 'channels-conflict')).toMatch(/Your selection \(SMS\) was not saved/)
    // Controls are locked until the user reloads; the stale write is not retried.
    expect(wrapper.find('[data-testid="channel-selector"]').attributes('disabled')).toBeDefined()
    await settle()
    expect(fake.callsTo('PUT', /\/channels$/)).toHaveLength(1)

    await wrapper.find('[data-testid="conflict-reload"]').trigger('click')
    await until(() => expect(wrapper.find('[data-testid="unsaved-channels"]').exists()).toBe(true))
    // The latest server state (sms, version 3) is shown, and the user's choice is offered.
    expect(text(wrapper, 'unsaved-channels')).toMatch(/Saved channels: SMS\. Your unsaved selection was SMS\./)
    expect(isChecked('sms')).toBe(true)
    expect(isChecked('email')).toBe(false)
    await until(() => expect(text(wrapper, 'kpi-total')).toMatch(/251/))

    // Re-apply is explicit; here the selection already equals the saved one.
    await wrapper.find('[data-testid="reapply-channels"]').trigger('click')
    await settle()
    expect(fake.callsTo('PUT', /\/channels$/)).toHaveLength(1)
    expect(wrapper.find('[data-testid="unsaved-channels"]').exists()).toBe(false)
  })

  it('409 version_conflict: re-applying a different selection uses the reloaded version', async () => {
    fake.seed(segmentedCampaign())
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    const { wrapper } = app
    await until(() => expect(wrapper.find('[data-testid="audience-stats"]').exists()).toBe(true))
    fake.externalChange(CAMPAIGN_ID, { name: 'Renamed elsewhere' }) // version 3, channels unchanged

    await checkbox('sms').setValue(false) // wants email only
    await until(() => expect(wrapper.find('[data-testid="channels-conflict"]').exists()).toBe(true))
    // The checkbox shows the saved state again, not a fake "saved" selection.
    expect(isChecked('sms')).toBe(true)

    await wrapper.find('[data-testid="conflict-reload"]').trigger('click')
    await until(() => expect(text(wrapper, 'campaign-name')).toBe('Renamed elsewhere'))
    expect(text(wrapper, 'unsaved-channels')).toMatch(/Your unsaved selection was Email/)
    await wrapper.find('[data-testid="reapply-channels"]').trigger('click')
    await until(() => expect(fake.callsTo('PUT', /\/channels$/)).toHaveLength(2))
    expect(fake.callsTo('PUT', /\/channels$/)[1]!.body).toEqual({ channels: ['email'], expected_version: 3 })
    await until(() => expect(text(wrapper, 'kpi-total')).toMatch(/318/))
    expect(wrapper.find('[data-testid="unsaved-channels"]').exists()).toBe(false)
  })

  it('re-interpretation does not override the user channel choice', async () => {
    fake.seed(segmentedCampaign({ channels: [], version: 3 }))
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    const { wrapper } = app
    await until(() => expect(wrapper.find('[data-testid="no-channels"]').exists()).toBe(true))
    await wrapper.find('[data-testid="interpret-button"]').trigger('click')
    await until(() => expect(fake.callsTo('POST', /\/interpret$/)).toHaveLength(1))
    await settle()
    expect(isChecked('sms')).toBe(false)
    expect(isChecked('email')).toBe(false)
    expect(fake.callsTo('PUT', /\/channels$/)).toHaveLength(0)
  })
})

describe('interpretation errors', () => {
  async function interpretWith(status: number, body: unknown) {
    fake.seed(draftCampaign())
    fake.override('POST', /\/interpret$/, () => json(status, body))
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    await until(() => expect(app!.wrapper.find('[data-testid="interpret-button"]').exists()).toBe(true))
    await app.wrapper.find('[data-testid="interpret-button"]').trigger('click')
    await until(() => expect(app!.wrapper.find('[data-testid="error-notice"]').exists()).toBe(true))
    return app.wrapper
  }

  it('invalid_brief shows field-level details and offers revising the brief', async () => {
    const wrapper = await interpretWith(
      422,
      errorEnvelope('invalid_brief', 'the brief cannot be turned into a supported audience rule; edit the brief', {
        details: [
          { path: 'brief', code: 'unsupported_targeting', message: 'loyalty tier is not a supported attribute' },
          { path: 'brief', code: 'unsupported_channel', message: 'WhatsApp is not a supported channel', allowed: ['sms', 'email'] },
        ],
      }),
    )
    expect(text(wrapper, 'error-notice')).toMatch(/The brief cannot be turned into a supported audience/)
    const items = wrapper.findAll('[data-testid="field-error"]').map((e) => spacedText(e.element))
    expect(items).toEqual([
      'brief loyalty tier is not a supported attribute (unsupported_targeting)',
      'brief WhatsApp is not a supported channel (unsupported_channel) Allowed: sms email',
    ])
    // Not retryable: no retry button; nothing was saved.
    expect(wrapper.text()).not.toMatch(/Retry interpretation/)
    expect(text(wrapper, 'status-badge')).toBe('Draft')

    const revise = wrapper.findAll('button').find((b) => b.text() === 'Revise brief in a new campaign')!
    await revise.trigger('click')
    await until(() => expect(app!.router.currentRoute.value.name).toBe('campaign-new'))
    await until(() =>
      expect((app!.wrapper.find('#campaign-brief').element as HTMLTextAreaElement).value).toBe(EXAMPLE_BRIEF),
    )
  })

  it('llm_refused explains the refusal without a retry', async () => {
    const wrapper = await interpretWith(
      422,
      errorEnvelope('llm_refused', 'the AI model declined to interpret this brief; rephrase the brief and try again'),
    )
    expect(text(wrapper, 'error-notice')).toMatch(/The AI declined to interpret this brief/)
    expect(text(wrapper, 'error-notice')).toMatch(/Nothing was saved/)
    expect(wrapper.text()).not.toMatch(/Retry interpretation/)
  })

  it('llm_unavailable is retryable and the retry succeeds', async () => {
    const wrapper = await interpretWith(
      503,
      errorEnvelope('llm_unavailable', 'the AI service is unavailable or timed out; try again later', { retryable: true }),
    )
    expect(text(wrapper, 'error-notice')).toMatch(/The AI service is unavailable/)
    fake.overrides = []
    const retry = wrapper.findAll('button').find((b) => b.text() === 'Retry interpretation')!
    await retry.trigger('click')
    await until(() => expect(wrapper.find('[data-testid="interpretation-card"]').exists()).toBe(true))
    expect(wrapper.find('[data-testid="error-notice"]').exists()).toBe(false)
  })

  it.each([
    ['llm_truncated', 'the AI answer was incomplete (output limit reached); shorten the brief or try again', /incomplete/],
    ['llm_rejected', 'the AI service rejected the request; check the server configuration', /rejected the request/],
  ])('502 %s', async (code, message, re) => {
    const wrapper = await interpretWith(502, errorEnvelope(code, message))
    expect(text(wrapper, 'error-notice')).toMatch(re)
  })

  it('version_conflict on interpret offers reloading', async () => {
    const wrapper = await interpretWith(
      409,
      errorEnvelope('version_conflict', 'the campaign was changed by another request; reload it and try again'),
    )
    expect(text(wrapper, 'error-notice')).toMatch(/changed while the AI was working/)
    expect(wrapper.findAll('button').some((b) => b.text() === 'Reload latest campaign')).toBe(true)
  })

  it('ai_output_invalid shows raw_proposal escaped inside a collapsed diagnostic section', async () => {
    const hostile =
      '{"segment":{"match":"all","conditions":[{"type":"attribute","field":"<img src=x onerror=alert(1)>"}]}}<script>window.__pwned=1</script>'
    const wrapper = await interpretWith(
      422,
      errorEnvelope('ai_output_invalid', 'the AI proposal was invalid after one repair attempt; nothing was saved', {
        details: [{ path: 'segment.conditions[0].field', code: 'unknown_field', message: 'unknown field' }],
        raw_proposal: hostile,
      }),
    )
    const details = wrapper.find('[data-testid="raw-proposal"]')
    expect(details.exists()).toBe(true)
    expect(details.element.tagName).toBe('DETAILS')
    expect((details.element as HTMLDetailsElement).open).toBe(false)
    expect(details.find('summary').text()).toMatch(/untrusted/)
    const pre = details.find('pre')
    expect(pre.text()).toBe(hostile) // shown verbatim as text
    expect(pre.element.children).toHaveLength(0) // no parsed elements
    expect(document.querySelector('img[src="x"]')).toBeNull()
    expect(document.querySelectorAll('script')).toHaveLength(0)
    expect((window as unknown as { __pwned?: number }).__pwned).toBeUndefined()
    expect(pre.html()).toContain('&lt;script&gt;')
  })
})

describe('audience errors', () => {
  it('interpretation saved but audience unavailable: explains and retries GET /audience only', async () => {
    fake.seed(draftCampaign())
    fake.interpretResult = (c) => fake.defaultInterpret(c, { audienceError: true })
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    const { wrapper } = app
    await until(() => expect(wrapper.find('[data-testid="interpret-button"]').exists()).toBe(true))
    await wrapper.find('[data-testid="interpret-button"]').trigger('click')
    await until(() => expect(wrapper.find('[data-testid="audience-after-interpret-error"]').exists()).toBe(true))

    expect(text(wrapper, 'audience-after-interpret-error')).toMatch(
      /The interpretation and targeting rule were saved\. Only the audience statistics could not be computed/,
    )
    // The interpretation is displayed as saved.
    expect(wrapper.find('[data-testid="interpretation-card"]').exists()).toBe(true)
    expect(wrapper.findAll('[data-testid="rule-condition"]')).toHaveLength(2)
    expect(text(wrapper, 'status-badge')).toBe('Audience ready')

    await wrapper.find('[data-testid="retry-audience"]').trigger('click')
    await until(() => expect(text(wrapper, 'kpi-matched')).toMatch(/412/))
    expect(fake.callsTo('GET', /\/audience$/)).toHaveLength(1)
    expect(fake.callsTo('POST', /\/interpret$/)).toHaveLength(1) // never re-interpreted
    expect(wrapper.find('[data-testid="audience-after-interpret-error"]').exists()).toBe(false)
  })

  it('audience_unavailable on GET /audience is retryable', async () => {
    fake.seed(segmentedCampaign())
    let fail = true
    fake.override('GET', /\/audience$/, () =>
      fail
        ? json(503, errorEnvelope('audience_unavailable', 'audience statistics are temporarily unavailable; try again', { retryable: true }))
        : undefined,
    )
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    const { wrapper } = app
    await until(() => expect(text(wrapper, 'error-notice')).toMatch(/Audience statistics are temporarily unavailable/))
    fail = false
    const retry = wrapper.findAll('button').find((b) => b.text() === 'Retry audience preview')!
    await retry.trigger('click')
    await until(() => expect(text(wrapper, 'kpi-matched')).toMatch(/412/))
  })
})

describe('async ordering', () => {
  it('an older audience response never overwrites newer channel state', async () => {
    fake.seed(segmentedCampaign())
    const first = deferred<Response>()
    let audienceCalls = 0
    fake.override('GET', /\/audience$/, () => {
      audienceCalls++
      // The initial audience request (version 2, both channels) is held back.
      return audienceCalls === 1 ? first.promise : undefined
    })
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    const { wrapper } = app
    await until(() => expect(wrapper.find('[data-testid="audience-loading"]').exists()).toBe(true))

    await checkbox('email').setValue(false)
    await until(() => expect(text(wrapper, 'kpi-total')).toMatch(/251/)) // sms only, version 3

    // The stale response for version 2 arrives late.
    first.resolve(
      json(200, {
        campaign_id: CAMPAIGN_ID,
        campaign_version: 2,
        segment_version: 1,
        channels: ['sms', 'email'],
        audience: audienceFor(['sms', 'email']),
      }),
    )
    await settle()
    expect(text(wrapper, 'kpi-total')).toMatch(/251/)
    expect(wrapper.find('[data-testid="partition-email"]').exists()).toBe(false)
    expect(isChecked('email')).toBe(false)
  })

  it('an audience response for a newer version (changed elsewhere) is not mixed in', async () => {
    fake.seed(segmentedCampaign())
    fake.override('GET', /\/audience$/, () =>
      json(200, {
        campaign_id: CAMPAIGN_ID,
        campaign_version: 9,
        segment_version: 1,
        channels: ['sms'],
        audience: audienceFor(['sms']),
      }),
    )
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    const { wrapper } = app
    await until(() => expect(wrapper.find('[data-testid="stale-notice"]').exists()).toBe(true))
    expect(wrapper.find('[data-testid="audience-stats"]').exists()).toBe(false)
  })

  it('navigating to another campaign drops the pending responses of the previous one', async () => {
    fake.seed(segmentedCampaign())
    fake.seed(segmentedCampaign({ id: OTHER_ID, name: 'Second campaign', channels: ['email'] }))
    const slow = deferred<Response>()
    fake.override('GET', new RegExp(`/campaigns/${CAMPAIGN_ID}$`), () => slow.promise)
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    const { wrapper, router } = app
    await router.push(`/campaigns/${OTHER_ID}`)
    await until(() => expect(text(wrapper, 'campaign-name')).toBe('Second campaign'))
    slow.resolve(json(200, segmentedCampaign()))
    await settle()
    expect(text(wrapper, 'campaign-name')).toBe('Second campaign')
    await until(() => expect(text(wrapper, 'kpi-total')).toMatch(/318/))
  })

  it('leaving the page aborts the in-flight interpretation and ignores its result', async () => {
    fake.seed(draftCampaign())
    const gate = deferred<Response>()
    let aborted = false
    fake.override('POST', /\/interpret$/, (req) => {
      req.signal?.addEventListener('abort', () => (aborted = true))
      return gate.promise
    })
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    const { wrapper, router } = app
    await until(() => expect(wrapper.find('[data-testid="interpret-button"]').exists()).toBe(true))
    await wrapper.find('[data-testid="interpret-button"]').trigger('click')
    await settle(1)
    await router.push('/campaigns/new')
    await settle()
    expect(aborted).toBe(true)
    gate.resolve(fake.defaultInterpret(fake.campaigns.get(CAMPAIGN_ID)!))
    await settle()
    expect(wrapper.find('[data-testid="brief-form"]').exists()).toBe(true)
  })
})
