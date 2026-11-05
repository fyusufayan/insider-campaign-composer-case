// A controllable EventSource for tests (installed with vi.stubGlobal). The
// test opens, feeds and fails connections explicitly; nothing reconnects by
// itself (the composable owns reconnection).
import type { EventSourceLike } from '@/composables/useCampaignEvents'

export class FakeEventSource implements EventSourceLike {
  static instances: FakeEventSource[] = []
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSED = 2

  readyState = 0
  onopen: ((ev: Event) => void) | null = null
  onerror: ((ev: Event) => void) | null = null
  closed = false
  private listeners = new Map<string, Array<(ev: MessageEvent) => void>>()

  constructor(readonly url: string) {
    FakeEventSource.instances.push(this)
  }

  static reset(): void {
    FakeEventSource.instances = []
  }

  /** Open connections (not closed by the client). */
  static open(): FakeEventSource[] {
    return FakeEventSource.instances.filter((s) => !s.closed)
  }

  static latest(): FakeEventSource {
    const s = FakeEventSource.instances[FakeEventSource.instances.length - 1]
    if (!s) throw new Error('no EventSource was created')
    return s
  }

  addEventListener(type: string, listener: (ev: MessageEvent) => void): void {
    const list = this.listeners.get(type) ?? []
    list.push(listener)
    this.listeners.set(type, list)
  }

  close(): void {
    this.closed = true
    this.readyState = 2
  }

  connect(): void {
    this.readyState = 1
    this.onopen?.(new Event('open'))
  }

  /** Dispatches an event (data: an object is JSON-encoded, a string is sent verbatim). */
  emit(name: string, data: unknown): void {
    const text = typeof data === 'string' ? data : JSON.stringify(data)
    for (const l of this.listeners.get(name) ?? []) l(new MessageEvent(name, { data: text }))
  }

  /** A network error / server close. */
  fail(): void {
    this.readyState = 0
    this.onerror?.(new Event('error'))
  }
}
