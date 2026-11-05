// The content step against the stateful fake backend: generation stream,
// editor validation, save, conflicts, navigation races and refresh.
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import type { Campaign, Channel } from '@/api/types'
import { useCampaignStore } from '@/stores/campaign'
import { FakeBackend, deferred, json, type FakeSseStream } from './helpers/fakeBackend'
import {
  CAMPAIGN_ID,
  GEN_BODY,
  GEN_SMS,
  GEN_SUBJECT,
  OTHER_ID,
  errorEnvelope,
  segmentedCampaign,
} from './helpers/fixtures'
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

async function open(c: Campaign = segmentedCampaign()): Promise<MountedApp> {
  fake.seed(c)
  app = await mountApp(`/campaigns/${c.id}`)
  await until(() => expect(app!.wrapper.find('[data-testid="content-panel"]').exists()).toBe(true))
  return app
}

function find(testid: string) {
  return app!.wrapper.find(`[data-testid="${testid}"]`)
}

const META = (c: Campaign, fields = ['sms_body', 'email_subject', 'email_body']) => ({
  campaign_id: c.id,
  campaign_version: c.version,
  channels: c.channels,
  fields,
  prompt_version: 'content_v1',
  limits: { sms_body: 160, email_subject: 80, email_body: 5000 },
})

/** A generation the test drives step by step. */
function heldGeneration(): { stream: Promise<FakeSseStream>; campaign: () => Campaign } {
  const d = deferred<FakeSseStream>()
  let seen: Campaign | null = null
  fake.generation = (c, s) => {
    seen = c
    d.resolve(s)
  }
  return { stream: d.promise, campaign: () => seen! }
}

describe('content generation', () => {
  it('shows progress and an unvalidated live preview, then the validated version', async () => {
    await open()
    expect(text(app!.wrapper, 'content-empty')).toMatch(/No content yet/)
    const held = heldGeneration()
    await find('generate-button').trigger('click')
    const s = await held.stream
    const c = held.campaign()
    s.event('meta', META(c))
    s.event('status', { phase: 'thinking', attempt: 1 })
    await until(() => expect(text(app!.wrapper, 'generation-headline')).toMatch(/thinking/))
    s.event('status', { phase: 'generating', attempt: 1, reason: 'initial' })
    s.event('delta', { attempt: 1, field: 'sms_body', text: 'Hi {{first_', preview: true })
    s.event('delta', { attempt: 1, field: 'sms_body', text: 'name}} <b>!</b>', preview: true })
    await until(() => expect(text(app!.wrapper, 'live-sms_body')).toContain('Hi {{first_name}} <b>!</b>'))
    expect(text(app!.wrapper, 'generation-live')).toMatch(/not validated, not saved/)
    // The live text is escaped and never enters the editor.
    expect(find('live-sms_body').find('b').exists()).toBe(false)
    expect((find('sms-input').element as HTMLTextAreaElement).value).toBe('')
    expect(find('sms-input').attributes('disabled')).toBeDefined()
    expect(find('generate-button').attributes('disabled')).toBeDefined()

    // A repair attempt discards the previous attempt's preview.
    s.event('status', { phase: 'validating', attempt: 1 })
    s.event('status', { phase: 'repairing', attempt: 1, problems: 2 })
    await until(() => expect(text(app!.wrapper, 'generation-headline')).toMatch(/failed 2 checks/))
    s.event('status', { phase: 'generating', attempt: 2, reason: 'repair' })
    await until(() => expect(find('generation-live').exists()).toBe(false))
    expect(text(app!.wrapper, 'generation-attempt')).toMatch(/request 2/)
    s.event('delta', { attempt: 2, field: 'sms_body', text: GEN_SMS, preview: true })
    s.event('status', { phase: 'validating', attempt: 2 })
    s.event('status', { phase: 'persisting' })
    await until(() => expect(find('generation-step-persisting').classes()).toContain('active'))

    const { campaign, version } = fake.commitVersion(c, {
      origin: 'ai', model: 'demo', prompt_version: 'content_v1', sms_body: GEN_SMS, email_subject: GEN_SUBJECT, email_body: GEN_BODY,
    })
    s.event('validated', fake.savedPayload(campaign, version, true, ['Demo content generator (fixed templates, not Claude) produced this content.']))
    s.close()

    await until(() => expect(find('generation-done').exists()).toBe(true))
    expect((find('sms-input').element as HTMLTextAreaElement).value).toBe(GEN_SMS)
    expect((find('subject-input').element as HTMLInputElement).value).toBe(GEN_SUBJECT)
    expect(text(app!.wrapper, 'content-origin')).toMatch(/Demo generator · not a live model/)
    expect(text(app!.wrapper, 'content-version-number')).toBe('Version 1')
    expect(text(app!.wrapper, 'save-state')).toBe('All changes saved')
    expect(text(app!.wrapper, 'generation-note')).toMatch(/not Claude/)
    expect(text(app!.wrapper, 'sms-preview-text')).toBe('Hi Ayşe, your cart is waiting. Enjoy 30% off today.')
    expect(text(app!.wrapper, 'sms-counter-worst')).toBe('67 / 160') // as computed by the (fake) server
    expect(text(app!.wrapper, 'example-values')).toMatch(/fictional example values \(Ayşe Yılmaz, İstanbul\)/)
    expect(text(app!.wrapper, 'generate-button')).toBe('Regenerate content')
    expect(useCampaignStore().campaign?.version).toBe(3)
  })

  it('refuses duplicate clicks while a generation is pending', async () => {
    await open()
    const held = heldGeneration()
    await find('generate-button').trigger('click')
    await find('generate-button').trigger('click')
    await useCampaignStore().generateContent()
    await held.stream
    expect(fake.callsTo('POST', /generations$/)).toHaveLength(1)
  })

  it('shows an error event, keeps nothing from the preview, and reloads the saved state', async () => {
    await open()
    fake.generation = (c, s) => {
      s.event('meta', META(c))
      s.event('status', { phase: 'generating', attempt: 1, reason: 'initial' })
      s.event('delta', { attempt: 1, field: 'sms_body', text: 'Hi {{email}}', preview: true })
      s.event('error', {
        status: 422,
        error: { code: 'ai_output_invalid', message: 'the generated content was invalid after one repair attempt; nothing was saved', details: [{ path: 'sms_body', code: 'unknown_variable', message: 'unknown placeholder' }], retryable: false, raw_proposal: '{"sms_body":"Hi {{email}}"}' },
      })
      s.close()
    }
    const gets = fake.callsTo('GET', /campaigns\/[^/]+$/).length
    await find('generate-button').trigger('click')
    await until(() => expect(find('error-notice').exists()).toBe(true))
    expect(text(app!.wrapper, 'error-notice')).toMatch(/generated content failed validation/)
    expect(find('raw-proposal').exists()).toBe(true)
    expect(find('generation-live').exists()).toBe(false)
    expect((find('sms-input').element as HTMLTextAreaElement).value).toBe('')
    await until(() => expect(fake.callsTo('GET', /campaigns\/[^/]+$/).length).toBe(gets + 1))
    expect(text(app!.wrapper, 'content-empty')).toMatch(/No content yet/)
  })

  it('treats a dropped connection as interrupted and shows a commit that happened before it', async () => {
    await open()
    fake.generation = (c, s) => {
      s.event('meta', META(c))
      s.event('status', { phase: 'persisting' })
      fake.commitVersion(c, { origin: 'ai', model: 'demo', prompt_version: 'content_v1', sms_body: GEN_SMS, email_subject: GEN_SUBJECT, email_body: GEN_BODY })
      s.close() // the connection ends before "validated" arrives
    }
    await find('generate-button').trigger('click')
    await until(() => expect(text(app!.wrapper, 'error-notice')).toMatch(/Connection lost during generation/))
    // The reconciliation reload shows the version that was committed.
    await until(() => expect(text(app!.wrapper, 'content-version-number')).toBe('Version 1'))
    expect((find('sms-input').element as HTMLTextAreaElement).value).toBe(GEN_SMS)
  })

  it('cancels a running generation and aborts the request', async () => {
    await open()
    const held = heldGeneration()
    await find('generate-button').trigger('click')
    const s = await held.stream
    s.event('status', { phase: 'thinking', attempt: 1 })
    await until(() => expect(find('generation-cancel').exists()).toBe(true))
    await find('generation-cancel').trigger('click')
    await until(() => expect(find('generation-cancelled').exists()).toBe(true))
    expect(s.closed).toBe(true)
    expect(find('generate-button').attributes('disabled')).toBeUndefined()
  })

  it('drops the stream of a campaign the user navigated away from', async () => {
    await open()
    fake.seed(segmentedCampaign({ id: OTHER_ID, name: 'Other campaign' }))
    const held = heldGeneration()
    await find('generate-button').trigger('click')
    const s = await held.stream
    s.event('status', { phase: 'thinking', attempt: 1 })
    await until(() => expect(find('generation-progress').exists()).toBe(true))

    await app!.router.push(`/campaigns/${OTHER_ID}`)
    await until(() => expect(text(app!.wrapper, 'campaign-name')).toBe('Other campaign'))
    // The request was aborted on navigation (the server cancels the model call).
    expect(s.closed).toBe(true)
    // Even if the first campaign's generation committed, nothing of it shows here.
    const c = held.campaign()
    fake.commitVersion(c, { origin: 'ai', model: 'demo', prompt_version: 'content_v1', sms_body: GEN_SMS, email_subject: GEN_SUBJECT, email_body: GEN_BODY })
    await settle()
    expect(find('generation-progress').exists()).toBe(false)
    expect(text(app!.wrapper, 'content-empty')).toMatch(/No content yet/)
    expect(useCampaignStore().campaign?.id).toBe(OTHER_ID)
  })

  it('restores the persisted content after a refresh', async () => {
    await open()
    await find('generate-button').trigger('click')
    await until(() => expect(find('generation-done').exists()).toBe(true))
    app!.wrapper.unmount()
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    await until(() => expect(text(app!.wrapper, 'content-version-number')).toBe('Version 1'))
    expect((find('sms-input').element as HTMLTextAreaElement).value).toBe(GEN_SMS)
    expect(text(app!.wrapper, 'save-state')).toBe('All changes saved')
  })
})

describe('channel-specific editors', () => {
  it.each<[Channel[], boolean, boolean]>([
    [['sms'], true, false],
    [['email'], false, true],
    [['sms', 'email'], true, true],
  ])('%j shows sms=%s email=%s', async (channels, sms, email) => {
    await open(segmentedCampaign({ channels }))
    expect(find('sms-section').exists()).toBe(sms)
    expect(find('email-section').exists()).toBe(email)
    await find('generate-button').trigger('click')
    await until(() => expect(find('generation-done').exists()).toBe(true))
    expect(find('sms-preview-text').exists()).toBe(sms)
    expect(find('email-preview-frame').exists()).toBe(email)
    expect(fake.contents.get(CAMPAIGN_ID)![0]!.sms_body !== null).toBe(sms)
  })

  it('shows no editor when no channel is selected', async () => {
    await open(segmentedCampaign({ channels: [] }))
    expect(find('no-channels').exists()).toBe(true)
    expect(find('generate-button').attributes('disabled')).toBeDefined()
  })
})

describe('editing and saving', () => {
  async function withContent() {
    await open()
    await find('generate-button').trigger('click')
    await until(() => expect(find('generation-done').exists()).toBe(true))
  }

  it('validates edits on the server (debounced) with counters, inline errors and previews', async () => {
    await withContent()
    const long = 'ş'.repeat(150) + ' {{first_name}}'
    await find('sms-input').setValue(long)
    expect(text(app!.wrapper, 'save-state')).toBe('Unsaved changes')
    await until(() => expect(text(app!.wrapper, 'sms-counter-worst')).toBe('171 / 160'), 3000)
    expect(find('sms-counter').attributes('data-state')).toBe('over')
    expect(text(app!.wrapper, 'sms-messages')).toMatch(/too_long/)
    expect(text(app!.wrapper, 'sms-preview-unavailable')).toMatch(/Fix the errors/)
    expect(find('save-button').attributes('disabled')).toBeDefined()
    // Only the latest draft is validated (rapid typing is debounced).
    const before = fake.callsTo('POST', /validate$/).length
    await find('sms-input').setValue('Hi {{city}}')
    await find('sms-input').setValue('Hi {{city}}!')
    await until(() => expect(text(app!.wrapper, 'sms-preview-text')).toBe('Hi İstanbul!'), 3000)
    const sent = fake.callsTo('POST', /validate$/).slice(before)
    expect(sent).toHaveLength(1)
    expect(sent[0]!.body).toEqual({ sms_body: 'Hi {{city}}!', email_subject: GEN_SUBJECT, email_body: GEN_BODY })
    expect(fake.contents.get(CAMPAIGN_ID)).toHaveLength(1) // validation never saves
  })

  it('saves an edit as a new version and treats unchanged text as a no-op', async () => {
    await withContent()
    await find('sms-input').setValue('Hi {{first_name|there}}! 30% off your cart today.')
    await until(() => expect(find('save-button').attributes('disabled')).toBeUndefined(), 3000)
    await find('save-button').trigger('click')
    await until(() => expect(text(app!.wrapper, 'content-origin')).toBe('Edited manually'))
    expect(text(app!.wrapper, 'content-version-number')).toBe('Version 2')
    const put = fake.callsTo('PUT', /content$/)[0]!
    expect(put.body).toEqual({
      sms_body: 'Hi {{first_name|there}}! 30% off your cart today.',
      email_subject: GEN_SUBJECT,
      email_body: GEN_BODY,
      expected_version: 3,
    })
    expect(useCampaignStore().campaign?.version).toBe(4)

    // Whitespace-only change: the server reports "unchanged", no new version.
    await find('sms-input').setValue('  Hi {{first_name|there}}! 30% off your cart today.  ')
    await until(() => expect(find('save-button').attributes('disabled')).toBeUndefined(), 3000)
    await find('save-button').trigger('click')
    await until(() => expect(text(app!.wrapper, 'save-state')).toMatch(/No changes/))
    expect(fake.contents.get(CAMPAIGN_ID)).toHaveLength(2)
    expect(useCampaignStore().campaign?.version).toBe(4)
  })

  it('sends null for fields of unselected channels', async () => {
    await open(segmentedCampaign({ channels: ['sms'] }))
    await find('sms-input').setValue('Hello {{first_name|there}}')
    await until(() => expect(find('save-button').attributes('disabled')).toBeUndefined(), 3000)
    await find('save-button').trigger('click')
    await until(() => expect(fake.callsTo('PUT', /content$/)).toHaveLength(1))
    expect(fake.callsTo('PUT', /content$/)[0]!.body).toEqual({
      sms_body: 'Hello {{first_name|there}}',
      email_subject: null,
      email_body: null,
      expected_version: 2,
    })
  })

  it('keeps the draft on a 409 conflict and saves it after an explicit reload', async () => {
    await withContent()
    await find('sms-input').setValue('My edit 30% off')
    await until(() => expect(find('save-button').attributes('disabled')).toBeUndefined(), 3000)
    fake.externalChange(CAMPAIGN_ID, {}) // another tab bumped the version
    await find('save-button').trigger('click')
    await until(() => expect(text(app!.wrapper, 'error-notice')).toMatch(/Your edits are kept/))
    expect((find('sms-input').element as HTMLTextAreaElement).value).toBe('My edit 30% off')
    expect(fake.contents.get(CAMPAIGN_ID)).toHaveLength(1)
    // Never retried automatically.
    expect(fake.callsTo('PUT', /content$/)).toHaveLength(1)

    await find('error-notice').findAll('button').find((b) => b.text().includes('Reload'))!.trigger('click')
    await until(() => expect(find('error-notice').exists()).toBe(false))
    expect((find('sms-input').element as HTMLTextAreaElement).value).toBe('My edit 30% off')
    await until(() => expect(find('save-button').attributes('disabled')).toBeUndefined(), 3000)
    await find('save-button').trigger('click')
    await until(() => expect(text(app!.wrapper, 'content-version-number')).toBe('Version 2'))
  })

  it('shows server field errors of a rejected save', async () => {
    await withContent()
    fake.override('PUT', /content$/, () =>
      json(422, errorEnvelope('validation_failed', 'the content is invalid; nothing was saved', {
        details: [{ path: 'email_body', code: 'markup_not_allowed', message: 'markup is not allowed' }],
      })),
    )
    await find('sms-input').setValue('Changed 30% off')
    await until(() => expect(find('save-button').attributes('disabled')).toBeUndefined(), 3000)
    await find('save-button').trigger('click')
    await until(() => expect(find('save-invalid').exists()).toBe(true))
    expect(text(app!.wrapper, 'body-messages')).toMatch(/markup_not_allowed/)
  })

  it('discards a save response that arrives after navigating away', async () => {
    await withContent()
    fake.seed(segmentedCampaign({ id: OTHER_ID, name: 'Other campaign' }))
    const gate = deferred<void>()
    fake.override('PUT', /content$/, async () => {
      await gate.promise
      return undefined // fall through to the real handler
    })
    await find('sms-input').setValue('Late edit 30% off')
    await until(() => expect(find('save-button').attributes('disabled')).toBeUndefined(), 3000)
    await find('save-button').trigger('click')
    await app!.router.push(`/campaigns/${OTHER_ID}`)
    await until(() => expect(text(app!.wrapper, 'campaign-name')).toBe('Other campaign'))
    gate.resolve()
    await settle()
    expect(useCampaignStore().campaign?.id).toBe(OTHER_ID)
    expect(find('content-empty').exists()).toBe(true)
  })

  it('blocks generation while there are unsaved edits', async () => {
    await withContent()
    await find('sms-input').setValue('Edited')
    expect(find('generate-button').attributes('disabled')).toBeDefined()
    expect(text(app!.wrapper, 'generate-hint')).toMatch(/Save or discard/)
    await find('discard-button').trigger('click')
    expect((find('sms-input').element as HTMLTextAreaElement).value).toBe(GEN_SMS)
    expect(find('generate-button').attributes('disabled')).toBeUndefined()
  })
})

describe('offer warnings on manual text (advisory, never lost)', () => {
  it('shows them while editing, keeps them after saving and after a reload', async () => {
    await open()
    await find('generate-button').trigger('click')
    await until(() => expect(find('generation-done').exists()).toBe(true))
    expect(find('offer-review').exists()).toBe(false)
    expect(text(app!.wrapper, 'offer-disclaimer')).toMatch(/keyword-based and advisory/)

    await find('sms-input').setValue('Free shipping on your cart today!')
    await until(() => expect(text(app!.wrapper, 'sms-messages')).toMatch(/unauthorized_offer/), 3000)
    expect(text(app!.wrapper, 'offer-review')).toMatch(/nothing is approved by saving it/)
    // A warning does not block saving.
    expect(find('save-button').attributes('disabled')).toBeUndefined()
    await find('save-button').trigger('click')
    await until(() => expect(text(app!.wrapper, 'content-version-number')).toBe('Version 2'))
    expect(text(app!.wrapper, 'sms-messages')).toMatch(/unauthorized_offer/)
    expect(find('offer-review').exists()).toBe(true)

    app!.wrapper.unmount()
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    await until(() => expect(text(app!.wrapper, 'sms-messages')).toMatch(/unauthorized_offer/))
    expect(find('offer-review').exists()).toBe(true)
  })

  it('cannot save before the server validated the current draft', async () => {
    await open()
    await find('generate-button').trigger('click')
    await until(() => expect(find('generation-done').exists()).toBe(true))
    const gate = deferred<void>()
    fake.override('POST', /validate$/, async () => {
      await gate.promise
      return undefined
    })
    await find('sms-input').setValue('Free shipping on your cart today!')
    await settle()
    expect(find('save-button').attributes('disabled')).toBeDefined()
    await useCampaignStore().saveContent() // the button is the guard; the store saves only on request
    gate.resolve()
    await until(() => expect(text(app!.wrapper, 'sms-messages')).toMatch(/unauthorized_offer/), 3000)
    expect(find('save-button').attributes('disabled')).toBeUndefined()
  })

  it('unmounting mid-generation aborts the stream and applies nothing', async () => {
    await open()
    const held = heldGeneration()
    await find('generate-button').trigger('click')
    const s = await held.stream
    s.event('status', { phase: 'thinking', attempt: 1 })
    app!.wrapper.unmount()
    app = null
    expect(s.closed).toBe(true)
    expect(useCampaignStore().campaign).toBeNull()
  })
})
