// Keyboard focus of modal dialogs (CP18): focus enters the dialog, Tab and
// Shift+Tab cycle inside it and never reach the page behind it, and focus
// returns to the opener when the dialog closes.
import { afterEach, describe, expect, it } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { nextTick } from 'vue'
import ConfirmDialog from '@/components/ui/ConfirmDialog.vue'

let opener: HTMLButtonElement
let wrapper: VueWrapper | null = null

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  document.body.innerHTML = ''
})

function press(key: string, shiftKey = false): KeyboardEvent {
  const e = new KeyboardEvent('keydown', { key, shiftKey, bubbles: true, cancelable: true })
  ;(document.activeElement ?? document.body).dispatchEvent(e)
  return e
}

async function open(pending = false): Promise<HTMLElement> {
  opener = document.createElement('button')
  opener.textContent = 'Open'
  document.body.appendChild(opener)
  opener.focus()
  const host = document.createElement('div')
  document.body.appendChild(host)
  wrapper = mount(ConfirmDialog, {
    attachTo: host,
    props: { title: 'Cancel sending?', confirmLabel: 'Cancel sending', pendingLabel: 'Cancelling…', pending, testid: 'cd' },
  })
  await nextTick()
  await nextTick()
  return wrapper.get('[data-testid="cd"]').element as HTMLElement
}

describe('modal dialog focus', () => {
  it('moves focus into the dialog on open', async () => {
    const dialog = await open()
    expect(document.activeElement).toBe(dialog)
  })

  it('wraps Tab and Shift+Tab inside the dialog', async () => {
    const dialog = await open()
    const buttons = Array.from(dialog.querySelectorAll('button'))
    expect(buttons.length).toBe(2)
    const [first, last] = [buttons[0]!, buttons[buttons.length - 1]!]

    // Shift+Tab from the dialog container goes to the last control, not the page.
    expect(press('Tab', true).defaultPrevented).toBe(true)
    expect(document.activeElement).toBe(last)
    // Tab from the last control wraps to the first.
    expect(press('Tab').defaultPrevented).toBe(true)
    expect(document.activeElement).toBe(first)
    // Shift+Tab from the first wraps to the last.
    press('Tab', true)
    expect(document.activeElement).toBe(last)
    expect(dialog.contains(document.activeElement)).toBe(true)
  })

  it('pulls focus back in if it is outside the dialog', async () => {
    const dialog = await open()
    opener.focus()
    press('Tab')
    expect(dialog.contains(document.activeElement)).toBe(true)
  })

  it('skips disabled controls while pending and keeps focus inside', async () => {
    const dialog = await open(true)
    press('Tab')
    expect(dialog.contains(document.activeElement)).toBe(true)
    expect((document.activeElement as HTMLButtonElement).disabled ?? false).toBe(false)
  })

  it('closes on Escape when nothing is pending, and not while pending', async () => {
    await open()
    press('Escape')
    expect(wrapper!.emitted('close')).toHaveLength(1)
    wrapper!.unmount()
    wrapper = null
    document.body.innerHTML = ''
    await open(true)
    press('Escape')
    expect(wrapper!.emitted('close')).toBeUndefined()
  })

  it('does not focus an opener that was removed (navigation)', async () => {
    await open()
    opener.remove()
    wrapper!.unmount()
    wrapper = null
    expect(document.activeElement).not.toBe(opener)
  })

  it('returns focus to the opener on close', async () => {
    await open()
    wrapper!.unmount()
    wrapper = null
    expect(document.activeElement).toBe(opener)
  })
})
