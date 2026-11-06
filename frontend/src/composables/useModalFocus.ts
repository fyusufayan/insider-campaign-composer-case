// Keyboard focus handling for modal dialogs (aria-modal="true"):
//  - focus moves into the dialog when it opens;
//  - Tab / Shift+Tab cycle within the dialog and never reach the page behind it;
//  - when the dialog closes, focus returns to the element that opened it (if
//    it is still in the document).
import { nextTick, onBeforeUnmount, onMounted, type Ref } from 'vue'

const FOCUSABLE = [
  'a[href]',
  'button:not([disabled])',
  'input:not([disabled]):not([type="hidden"])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  'iframe',
  '[tabindex]:not([tabindex="-1"])',
].join(',')

function focusableIn(root: HTMLElement): HTMLElement[] {
  return Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE)).filter((el) => !el.closest('[hidden], [inert]'))
}

/** Returns a keydown handler step that keeps Tab focus inside the dialog. */
export function useModalFocus(dialog: Ref<HTMLElement | null>): { trapTab: (e: KeyboardEvent) => void } {
  let opener: HTMLElement | null = null

  function trapTab(e: KeyboardEvent): void {
    const root = dialog.value
    if (e.key !== 'Tab' || !root) return
    const items = focusableIn(root)
    if (items.length === 0) {
      e.preventDefault()
      root.focus()
      return
    }
    const first = items[0]!
    const last = items[items.length - 1]!
    const active = document.activeElement
    const inside = active instanceof HTMLElement && root.contains(active)
    if (e.shiftKey && (!inside || active === first || active === root)) {
      e.preventDefault()
      last.focus()
    } else if (!e.shiftKey && (!inside || active === last)) {
      e.preventDefault()
      first.focus()
    }
  }

  onMounted(async () => {
    opener = document.activeElement instanceof HTMLElement ? document.activeElement : null
    await nextTick()
    dialog.value?.focus()
  })
  onBeforeUnmount(() => {
    if (opener && opener.isConnected) opener.focus()
    opener = null
  })

  return { trapTab }
}
