// The approval step (CP15) against the stateful fake backend: prepare →
// review dialog → confirm / withdraw, offer findings, expiry, conflicts,
// navigation races, refresh, and strict decoding of approval responses.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Campaign, ContentVersion } from '@/api/types'
import { decodeApprovalResponse } from '@/api/decode'
import { useCampaignStore } from '@/stores/campaign'
import { FakeBackend, deferred, json } from './helpers/fakeBackend'
import {
  APPROVAL_ID,
  CAMPAIGN_ID,
  OTHER_ID,
  SERVER_TIME,
  approvalFor,
  contentVersion,
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
  vi.useRealTimers()
})

/** A content_ready campaign with a saved, valid content version. */
function readyCampaign(id = CAMPAIGN_ID): { campaign: Campaign; version: ContentVersion } {
  const c = fake.seed(segmentedCampaign({ id }))
  const { campaign, version } = fake.commitVersion(c, contentVersion())
  return { campaign, version }
}

async function open(id = CAMPAIGN_ID): Promise<MountedApp> {
  app = await mountApp(`/campaigns/${id}`)
  await until(() => expect(app!.wrapper.find('[data-testid="approval-panel"]').exists()).toBe(true))
  return app
}

function find(testid: string) {
  return app!.wrapper.find(`[data-testid="${testid}"]`)
}

async function prepare(): Promise<void> {
  await until(() => expect(find('prepare-approval').attributes('disabled')).toBeUndefined())
  await find('prepare-approval').trigger('click')
  await until(() => expect(find('approval-dialog').exists()).toBe(true))
}

async function acknowledge(): Promise<void> {
  await find('ack-snapshot').setValue(true)
  if (find('ack-findings').exists()) await find('ack-findings').setValue(true)
}

describe('prepare and review', () => {
  it('freezes the audience and shows the summary from the prepare response', async () => {
    const { campaign } = readyCampaign()
    await open()
    expect(text(app!.wrapper, 'approval-panel')).toMatch(/freezes the current content version/)
    await prepare()
    const post = fake.callsTo('POST', /\/approvals$/)
    expect(post).toHaveLength(1)
    expect(post[0]!.body).toEqual({ expected_version: campaign.version })

    const store = useCampaignStore()
    expect(store.campaign?.status).toBe('pending_approval')
    expect(store.campaign?.version).toBe(campaign.version + 1)
    // Frozen counts per channel (eligible from the stored partition), totals, binding.
    expect(text(app!.wrapper, 'approval-row-sms')).toMatch(/^SMS 251 140 21$/)
    expect(text(app!.wrapper, 'approval-row-email')).toMatch(/^Email 318 82 12$/)
    expect(text(app!.wrapper, 'approval-total')).toBe('569')
    expect(text(app!.wrapper, 'approval-reach')).toMatch(/202 on both channels/)
    expect(text(app!.wrapper, 'approval-content')).toMatch(/Version 1 · AI \(demo generator, not Claude\)/)
    expect(text(app!.wrapper, 'approval-segment')).toMatch(/Segment version 1/)
    expect(text(app!.wrapper, 'approval-channels')).toBe('SMS + Email')
    expect(text(app!.wrapper, 'approval-explainer')).toMatch(/consent and contact details are checked again/)
    expect(text(app!.wrapper, 'approval-countdown')).toMatch(/1[45]:\d\d/)
    // Rendered with the fictional example values, escaped, in previews.
    expect(find('approval-previews').exists()).toBe(true)
    // No send action while the approval is only pending (Start sending appears once approved).
    expect(app!.wrapper.text()).not.toMatch(/\bSend\b/)
  })

  it('explains why prepare is unavailable and never prepares a dirty or invalid draft', async () => {
    readyCampaign()
    await open()
    await until(() => expect(find('prepare-approval').attributes('disabled')).toBeUndefined())
    const store = useCampaignStore()
    store.setDraftField('sms_body', 'changed text')
    await settle()
    expect(find('prepare-approval').attributes('disabled')).toBeDefined()
    expect(text(app!.wrapper, 'prepare-blocked')).toMatch(/Save or discard/)
    await store.prepareApproval()
    expect(fake.callsTo('POST', /\/approvals$/)).toHaveLength(0)
  })

  it('sends one prepare for repeated clicks', async () => {
    readyCampaign()
    const gate = deferred<void>()
    fake.override('POST', /\/approvals$/, async () => {
      await gate.promise
      return undefined
    })
    await open()
    await until(() => expect(find('prepare-approval').attributes('disabled')).toBeUndefined())
    await find('prepare-approval').trigger('click')
    await find('prepare-approval').trigger('click')
    const store = useCampaignStore()
    await store.prepareApproval()
    expect(find('prepare-approval').attributes('disabled')).toBeDefined()
    gate.resolve()
    await until(() => expect(find('approval-dialog').exists()).toBe(true))
    expect(fake.callsTo('POST', /\/approvals$/)).toHaveLength(1)
  })

  it('shows empty-audience and conflict errors from prepare without retrying', async () => {
    readyCampaign()
    fake.override('POST', /\/approvals$/, () =>
      json(422, errorEnvelope('empty_audience', 'no selected channel has an eligible recipient (contact and consent are required); nothing was prepared')),
    )
    await open()
    await until(() => expect(find('prepare-approval').attributes('disabled')).toBeUndefined())
    await find('prepare-approval').trigger('click')
    await until(() => expect(text(app!.wrapper, 'approval-panel')).toMatch(/Nobody can be messaged/))
    expect(fake.callsTo('POST', /\/approvals$/)).toHaveLength(1)
    expect(find('approval-dialog').exists()).toBe(false)
  })
})

describe('offer findings', () => {
  it('lists every finding, separates them from errors and requires an explicit acknowledgement', async () => {
    readyCampaign()
    fake.nextFindings = [
      { path: 'sms_body', code: 'unauthorized_offer', message: 'mentions a discount the content brief does not authorize' },
      { path: 'email_body', code: 'unauthorized_claim', message: 'states a monetary amount' },
    ]
    await open()
    await prepare()
    const section = text(app!.wrapper, 'approval-offer-findings')
    expect(section).toMatch(/2 advisory findings/)
    expect(section).toMatch(/keyword-based/)
    expect(text(app!.wrapper, 'approval-finding-0')).toMatch(/Offer sms_body mentions a discount/)
    expect(text(app!.wrapper, 'approval-finding-1')).toMatch(/Claim email_body states a monetary amount/)
    expect(text(app!.wrapper, 'approval-offer')).toBe('30% discount')
    // Confirm requires both acknowledgements.
    expect(find('approval-confirm').attributes('disabled')).toBeDefined()
    await find('ack-snapshot').setValue(true)
    expect(find('approval-confirm').attributes('disabled')).toBeDefined()
    await find('ack-findings').setValue(true)
    expect(find('approval-confirm').attributes('disabled')).toBeUndefined()
  })
})

describe('confirm', () => {
  it('confirms with the exact binding shown and shows the persisted approved state after refresh', async () => {
    readyCampaign()
    await open()
    await prepare()
    const store = useCampaignStore()
    const shown = store.approval!
    await acknowledge()
    await find('approval-confirm').trigger('click')
    await until(() => expect(store.campaign?.status).toBe('approved'))
    const [call] = fake.callsTo('POST', /\/confirm$/)
    expect(call!.path).toBe(`/api/v1/campaigns/${CAMPAIGN_ID}/approvals/${APPROVAL_ID}/confirm`)
    expect(call!.body).toEqual({
      expected_version: shown.campaign_version,
      segment_version: shown.segment_version,
      content_version: shown.content_version,
      channels: shown.channels,
    })
    expect(find('approval-dialog').exists()).toBe(false)
    expect(find('approval-notice').attributes('data-kind')).toBe('confirmed')
    expect(text(app!.wrapper, 'approval-approved')).toMatch(/Approved — not sent/)
    expect(text(app!.wrapper, 'approved-countdown')).toMatch(/2[34]:\d\d:\d\d/)
    expect(app!.wrapper.find('button').text()).not.toMatch(/Send/)

    // Refresh: a new app instance restores the approved summary from the server.
    app!.wrapper.unmount()
    app = await open()
    await until(() => expect(find('approval-approved').exists()).toBe(true))
    expect(text(app!.wrapper, 'approval-status')).toBe('Confirmed')
    expect(text(app!.wrapper, 'approval-approved-at')).toMatch(/by marketer@demo/)
    expect(find('approval-dialog').exists()).toBe(false)
  })

  it('sends a single confirm for repeated clicks', async () => {
    readyCampaign()
    const gate = deferred<void>()
    fake.override('POST', /\/confirm$/, async () => {
      await gate.promise
      return undefined
    })
    await open()
    await prepare()
    await acknowledge()
    await find('approval-confirm').trigger('click')
    await find('approval-confirm').trigger('click')
    await useCampaignStore().confirmApproval()
    expect(find('approval-confirm').attributes('disabled')).toBeDefined()
    gate.resolve()
    await until(() => expect(useCampaignStore().campaign?.status).toBe('approved'))
    expect(fake.callsTo('POST', /\/confirm$/)).toHaveLength(1)
  })

  it('shows a 409 after another tab changed the campaign and recovers by reloading', async () => {
    readyCampaign()
    await open()
    await prepare()
    fake.externalChange(CAMPAIGN_ID, {}) // another tab: version + 1
    await acknowledge()
    await find('approval-confirm').trigger('click')
    await until(() => expect(find('approval-dialog').text()).toMatch(/This campaign was changed elsewhere/))
    expect(find('approval-dialog').text()).toMatch(/Nothing was confirmed/)
    expect(fake.callsTo('POST', /\/confirm$/)).toHaveLength(1) // never retried
    const reload = find('approval-dialog').findAll('button').find((b) => b.text() === 'Reload latest campaign')!
    await reload.trigger('click')
    const store = useCampaignStore()
    await until(() => expect(store.approvalCurrent).toBe(true))
    expect(store.approvalActionError).toBeNull()
    expect(store.campaign?.status).toBe('pending_approval')
  })

  it('shows approval_expired from confirm and the released state', async () => {
    readyCampaign()
    await open()
    await prepare()
    await acknowledge()
    // The approval expires on the server just before the click lands.
    const a = fake.approvals.get(APPROVAL_ID)!
    fake.approvals.set(APPROVAL_ID, { ...a, expires_at: new Date(Date.now() - 1000).toISOString() })
    await find('approval-confirm').trigger('click')
    const store = useCampaignStore()
    await until(() => expect(store.approvalNotice).toBe('expired'))
    expect(store.campaign?.status).toBe('content_ready')
    expect(store.approval?.status).toBe('expired')
    expect(find('approval-dialog').exists()).toBe(false)
    expect(text(app!.wrapper, 'approval-notice')).toMatch(/Approval expired — review again/)
    expect(text(app!.wrapper, 'approval-action-error')).toMatch(/The approval has expired/)
    expect(find('prepare-approval').exists()).toBe(true)
  })
})

describe('expiry while the dialog is open', () => {
  it('disables confirm at the deadline and asks the server, which expires the approval', async () => {
    vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] })
    vi.setSystemTime(new Date('2026-11-04T10:00:00Z'))
    readyCampaign()
    await open()
    await prepare()
    await acknowledge()
    expect(find('approval-confirm').attributes('disabled')).toBeUndefined()
    const gets = fake.callsTo('GET', /\/approvals\/[^/]+$/).length
    vi.setSystemTime(new Date('2026-11-04T10:15:01Z'))
    vi.advanceTimersByTime(1000)
    const store = useCampaignStore()
    await until(() => expect(store.approvalNotice).toBe('expired'))
    expect(fake.callsTo('GET', /\/approvals\/[^/]+$/).length).toBe(gets + 1)
    expect(fake.callsTo('POST', /\/confirm$/)).toHaveLength(0)
    expect(store.campaign?.status).toBe('content_ready')
    expect(find('approval-dialog').exists()).toBe(false)
  })

  it('shows the expired state inside the dialog while the server answer is pending', async () => {
    vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] })
    vi.setSystemTime(new Date('2026-11-04T10:00:00Z'))
    readyCampaign()
    await open()
    await prepare()
    const gate = deferred<void>()
    fake.override('GET', /\/approvals\/[^/]+$/, async () => {
      await gate.promise
      return undefined
    })
    vi.setSystemTime(new Date('2026-11-04T10:15:01Z'))
    vi.advanceTimersByTime(1000)
    await until(() => expect(find('approval-expired-banner').exists()).toBe(true))
    expect(find('approval-confirm').attributes('disabled')).toBeDefined()
    expect(find('ack-snapshot').element.closest('fieldset')!.disabled).toBe(true)
    gate.resolve()
    await until(() => expect(useCampaignStore().approvalNotice).toBe('expired'))
  })
})

describe('withdraw and close', () => {
  it('withdraws a pending approval from the dialog', async () => {
    readyCampaign()
    await open()
    await prepare()
    await find('approval-withdraw').trigger('click')
    const store = useCampaignStore()
    await until(() => expect(store.approvalNotice).toBe('withdrawn'))
    expect(store.campaign?.status).toBe('content_ready')
    expect(store.campaign?.active_approval_id).toBeNull()
    expect(fake.callsTo('POST', /\/withdraw$/)[0]!.body).toEqual({ expected_version: store.campaign!.version - 1 })
    expect(find('approval-dialog').exists()).toBe(false)
    expect(find('prepare-approval').exists()).toBe(true)
  })

  it('withdraws a confirmed approval only after an explicit second step', async () => {
    readyCampaign()
    await open()
    await prepare()
    await acknowledge()
    await find('approval-confirm').trigger('click')
    await until(() => expect(find('approved-withdraw').exists()).toBe(true))
    await find('approved-withdraw').trigger('click')
    expect(fake.callsTo('POST', /\/withdraw$/)).toHaveLength(0)
    expect(text(app!.wrapper, 'approved-withdraw-confirm-text')).toMatch(/Withdraw the confirmed approval\?/)
    await find('approved-withdraw-confirm').trigger('click')
    await until(() => expect(useCampaignStore().approvalNotice).toBe('withdrawn'))
    expect(fake.callsTo('POST', /\/withdraw$/)).toHaveLength(1)
    expect(useCampaignStore().campaign?.status).toBe('content_ready')
  })

  it('closes without confirming: no request, the approval stays pending and can be resumed', async () => {
    readyCampaign()
    await open()
    await prepare()
    const before = fake.calls.length
    await find('approval-close').trigger('click')
    expect(find('approval-dialog').exists()).toBe(false)
    expect(fake.calls.length).toBe(before)
    expect(text(app!.wrapper, 'approval-pending')).toMatch(/waiting for your confirmation/)
    await find('open-approval-dialog').trigger('click')
    expect(find('approval-dialog').exists()).toBe(true)
    // Escape closes too.
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
    await settle()
    expect(find('approval-dialog').exists()).toBe(false)
  })

  it('restores the review dialog on refresh while pending', async () => {
    const { campaign, version } = readyCampaign()
    fake.seedPending({ ...campaign, version: campaign.version + 1 }, version)
    await open()
    await until(() => expect(find('approval-dialog').exists()).toBe(true))
    expect(text(app!.wrapper, 'approval-total')).toBe('569')
  })

  it('shows an approval that expired elsewhere as expired after a reload', async () => {
    const { campaign, version } = readyCampaign()
    fake.seedPending({ ...campaign, version: campaign.version + 1 }, version)
    await open()
    await until(() => expect(find('approval-dialog').exists()).toBe(true))
    await find('approval-close').trigger('click')
    const a = fake.approvals.get(APPROVAL_ID)!
    fake.approvals.set(APPROVAL_ID, { ...a, expires_at: new Date(Date.now() - 1).toISOString() })
    await useCampaignStore().reload()
    await until(() => expect(useCampaignStore().approvalNotice).toBe('expired'))
    expect(useCampaignStore().campaign?.status).toBe('content_ready')
  })
})

describe('navigation races', () => {
  it('drops a late prepare response after switching campaigns', async () => {
    readyCampaign()
    readyCampaign(OTHER_ID)
    const gate = deferred<void>()
    fake.override('POST', /\/approvals$/, async () => {
      await gate.promise
      return undefined
    })
    await open()
    await until(() => expect(find('prepare-approval').attributes('disabled')).toBeUndefined())
    await find('prepare-approval').trigger('click')
    await app!.router.push(`/campaigns/${OTHER_ID}`)
    const store = useCampaignStore()
    await until(() => expect(store.campaign?.id).toBe(OTHER_ID))
    gate.resolve()
    await settle()
    expect(store.campaign?.id).toBe(OTHER_ID)
    expect(store.approval).toBeNull()
    expect(store.preparing).toBe(false)
    expect(find('approval-dialog').exists()).toBe(false)
  })

  it('drops a late approval read after leaving the page', async () => {
    const { campaign, version } = readyCampaign()
    fake.seedPending({ ...campaign, version: campaign.version + 1 }, version)
    const gate = deferred<void>()
    fake.override('GET', /\/approvals\/[^/]+$/, async () => {
      await gate.promise
      return undefined
    })
    app = await mountApp(`/campaigns/${CAMPAIGN_ID}`)
    await until(() => expect(fake.callsTo('GET', /\/approvals\//)).toHaveLength(1))
    await app.router.push('/campaigns/new')
    gate.resolve()
    await settle()
    const store = useCampaignStore()
    expect(store.approval).toBeNull()
    expect(store.campaignId).toBeNull()
  })
})

describe('strict approval decoding', () => {
  function valid() {
    const c: Campaign = { ...segmentedCampaign(), status: 'pending_approval', active_approval_id: APPROVAL_ID, current_content_id: 101, version: 4 }
    return { campaign: c, approval: approvalFor(c, contentVersion()), server_time: SERVER_TIME }
  }

  it('accepts the contract', () => {
    expect(decodeApprovalResponse(JSON.parse(JSON.stringify(valid()))).approval.snapshot_count).toBe(569)
  })

  it.each([
    ['an unknown approval property (e.g. recipient values)', (r: any) => (r.approval.vars = [{ first_name: 'x' }])],
    ['an unknown snapshot property', (r: any) => (r.approval.snapshot.sms.user_ids = [1])],
    ['an unknown response property', (r: any) => (r.recipients = [])],
    ['another campaign', (r: any) => (r.approval.campaign_id = OTHER_ID)],
    ['another campaign version', (r: any) => (r.approval.campaign_version = 3)],
    ['a review on a closed approval', (r: any) => {
      r.approval.status = 'expired'
      r.campaign.status = 'content_ready'
      r.campaign.active_approval_id = null
    }],
    ['a live approval without review', (r: any) => (r.approval.review = null)],
    ['a count that is not the sum', (r: any) => (r.approval.snapshot_count = 570)],
    ['recipients that differ from the stored eligible count', (r: any) => (r.approval.snapshot.sms.recipients = 1)],
    ['a missing channel snapshot', (r: any) => delete r.approval.snapshot.email],
    ['a bad digest', (r: any) => (r.approval.snapshot_digest = 'xyz')],
    ['an unknown status', (r: any) => (r.approval.status = 'reopened')],
    ['revoked without a reason', (r: any) => {
      r.approval.status = 'revoked'
      r.approval.review = null
    }],
    ['an approved approval without approved_at', (r: any) => {
      r.approval.status = 'approved'
      r.campaign.status = 'approved'
    }],
    ['a pending approval of a campaign that is not pending_approval', (r: any) => (r.campaign.status = 'content_ready')],
    ['a live approval that is not the active one', (r: any) => (r.campaign.active_approval_id = null)],
    ['an invalid timestamp', (r: any) => (r.approval.expires_at = 'soon')],
    ['an invalid review', (r: any) => (r.approval.review.validation.valid = false)],
    ['a non-uuid approval id', (r: any) => (r.approval.approval_id = '1')],
  ])('rejects %s', (_name, mutate) => {
    const r = JSON.parse(JSON.stringify(valid()))
    mutate(r)
    expect(() => decodeApprovalResponse(r)).toThrow()
  })

  it('surfaces a contract violation as an invalid response, never as approval state', async () => {
    readyCampaign()
    fake.override('POST', /\/approvals$/, () => {
      const r = valid()
      return json(201, { ...r, approval: { ...r.approval, vars: [{ first_name: 'Zeynep' }] } })
    })
    await open()
    await until(() => expect(find('prepare-approval').attributes('disabled')).toBeUndefined())
    await find('prepare-approval').trigger('click')
    await until(() => expect(text(app!.wrapper, 'approval-panel')).toMatch(/Unexpected server response/))
    expect(useCampaignStore().approval).toBeNull()
    expect(app!.wrapper.text()).not.toContain('Zeynep')
  })
})

describe('privacy and escaping', () => {
  it('renders untrusted text escaped and shows no recipient-level data', async () => {
    const { campaign, version } = readyCampaign()
    fake.nextFindings = [{ path: 'sms_body', code: 'unauthorized_offer', message: '<img src=x onerror=alert(1)> offer' }]
    void campaign
    void version
    await open()
    await prepare()
    const finding = find('approval-finding-0')
    expect(finding.find('img').exists()).toBe(false)
    expect(spacedText(finding.element)).toContain('<img src=x onerror=alert(1)> offer')
    expect(app!.wrapper.html()).not.toMatch(/user_id|idempotency|destination/)
  })
})
