import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, type Router } from 'vue-router'
import { expect, vi } from 'vitest'
import App from '@/App.vue'
import { makeRouter } from '@/router'

export interface MountedApp {
  wrapper: VueWrapper
  router: Router
}

/** Mounts the whole application at `path` with an in-memory router. */
export async function mountApp(path: string): Promise<MountedApp> {
  const pinia = createPinia()
  setActivePinia(pinia)
  const router = makeRouter(createMemoryHistory())
  await router.push(path)
  await router.isReady()
  const wrapper = mount(App, { global: { plugins: [pinia, router] }, attachTo: document.body })
  await settle()
  return { wrapper, router }
}

/** Lets pending promises, lazy routes and re-renders complete. */
export async function settle(rounds = 5): Promise<void> {
  for (let i = 0; i < rounds; i++) {
    await flushPromises()
    await new Promise((r) => setTimeout(r, 0))
  }
}

/** Polls until the assertion passes (no fixed sleeps). */
export async function until(assertion: () => void, timeoutMs = 2000): Promise<void> {
  await vi.waitFor(
    async () => {
      await flushPromises()
      assertion()
    },
    { timeout: timeoutMs, interval: 5 },
  )
}

/** Visible-ish text of an element: text nodes joined by spaces, collapsed. */
export function spacedText(el: Element): string {
  const parts: string[] = []
  const walk = (n: Node) => {
    if (n.nodeType === Node.TEXT_NODE) parts.push(n.textContent ?? '')
    else n.childNodes.forEach(walk)
  }
  walk(el)
  return parts
    .join(' ')
    .replace(/\s+/g, ' ')
    .replace(/\s+([.,:)])/g, '$1')
    .replace(/\(\s+/g, '(')
    .trim()
}

export function text(w: VueWrapper, testid: string): string {
  const el = w.find(`[data-testid="${testid}"]`)
  expect(el.exists(), `missing [data-testid="${testid}"]`).toBe(true)
  return spacedText(el.element)
}
