import { describe, expect, it } from 'vitest'
import {
  DecodeError,
  decodeContentSaved,
  decodeContentValidation,
  decodeContentVersion,
  decodeCurrentContent,
  decodeGenerationEvent,
} from '@/api/decode'
import { contentVersion, currentContent, segmentedCampaign } from './helpers/fixtures'

function saved() {
  const c = segmentedCampaign({ status: 'content_ready', version: 3, current_content_id: 101 })
  return { campaign: c, content: currentContent(c, contentVersion()), created: true, notes: [] }
}

describe('content decoders', () => {
  it('accepts the documented shapes', () => {
    const s = saved()
    expect(decodeContentSaved(structuredClone(s)).content.content?.id).toBe(101)
    const empty = currentContent(segmentedCampaign(), null)
    expect(decodeCurrentContent(structuredClone(empty))).toEqual(empty)
  })

  it('rejects inconsistent or malformed content', () => {
    expect(() => decodeContentVersion({ ...contentVersion(), model: null })).toThrow(DecodeError)
    expect(() => decodeContentVersion({ ...contentVersion(), origin: 'edited' })).toThrow(DecodeError) // edited with a model
    expect(() => decodeContentVersion({ ...contentVersion(), origin: 'human' })).toThrow(DecodeError)
    expect(() => decodeContentVersion({ ...contentVersion(), sms_body: 5 })).toThrow(DecodeError)
    expect(() => decodeContentVersion({ ...contentVersion(), version: 0 })).toThrow(DecodeError)
    const v = currentContent(segmentedCampaign(), contentVersion()).validation!
    expect(() => decodeContentValidation({ ...v, valid: false })).toThrow(DecodeError)
    expect(() => decodeContentValidation({ ...v, errors: 'x' })).toThrow(DecodeError)
    expect(() => decodeCurrentContent({ ...currentContent(segmentedCampaign(), contentVersion()), validation: null })).toThrow(DecodeError)
  })

  it('binds a saved result to its campaign version and content id', () => {
    const s = saved()
    expect(() => decodeContentSaved({ ...s, content: { ...s.content, campaign_version: 2 } })).toThrow(DecodeError)
    expect(() => decodeContentSaved({ ...s, campaign: { ...s.campaign, current_content_id: 7 } })).toThrow(DecodeError)
    expect(() => decodeContentSaved({ ...s, content: { ...s.content, content: null, validation: null } })).toThrow(DecodeError)
  })

  it('decodes generation events strictly', () => {
    expect(decodeGenerationEvent('status', { phase: 'thinking', attempt: 1 })).toEqual({
      type: 'status',
      data: { phase: 'thinking', attempt: 1 },
    })
    expect(decodeGenerationEvent('delta', { attempt: 1, field: 'sms_body', text: 'Hi', preview: true }).type).toBe('delta')
    expect(() => decodeGenerationEvent('delta', { attempt: 1, field: 'sms_body', text: 'Hi', preview: false })).toThrow()
    expect(() => decodeGenerationEvent('delta', { attempt: 1, field: 'sql', text: 'Hi', preview: true })).toThrow()
    expect(() => decodeGenerationEvent('delta', { attempt: 1, field: 'sms_body', text: 'x'.repeat(5000), preview: true })).toThrow()
    expect(() => decodeGenerationEvent('status', { phase: 'done' })).toThrow()
    expect(() => decodeGenerationEvent('content', {})).toThrow()
    expect(decodeGenerationEvent('validated', saved()).type).toBe('validated')
    const err = decodeGenerationEvent('error', {
      status: 422,
      error: { code: 'ai_output_invalid', message: 'm', details: [], retryable: false, raw_proposal: '{"x":1}' },
    })
    expect(err).toMatchObject({ type: 'error', data: { status: 422, error: { raw_proposal: '{"x":1}' } } })
  })
})
