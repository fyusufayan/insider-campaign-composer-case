// Incremental parser for text/event-stream (WHATWG HTML "server-sent
// events" interpretation rules), used to read the content generation
// response of a fetch() POST, which EventSource cannot issue.
//
// - Chunks may split anywhere: inside a line, between "\r" and "\n", and
//   (when fed bytes) inside a multi-byte UTF-8 sequence.
// - Lines end with CRLF, LF or CR; a leading BOM is ignored.
// - "event", "data" (multi-line, joined with "\n"), "id" and "retry" fields;
//   comment lines (":") and unknown fields are ignored.
// - An event is dispatched on a blank line; an event without data is not
//   dispatched; the default event name is "message".
// - Memory is bounded: a line or an event larger than maxEventChars is an
//   error (SseLimitError) instead of unbounded buffering.

export interface SseMessage {
  event: string
  data: string
  id: string
}

export class SseLimitError extends Error {
  constructor(limit: number) {
    super(`server-sent event exceeds ${limit} characters`)
    this.name = 'SseLimitError'
  }
}

/** Default bound per event (the backend caps events at 256 KiB). */
export const DEFAULT_MAX_EVENT_CHARS = 1 << 20

export class SseParser {
  private readonly decoder = new TextDecoder('utf-8')
  private line = ''
  private pendingCR = false
  private started = false
  private eventName = ''
  private data: string[] = []
  private dataChars = 0
  private lastId = ''

  constructor(
    private readonly onMessage: (m: SseMessage) => void,
    private readonly maxEventChars = DEFAULT_MAX_EVENT_CHARS,
  ) {}

  /** Feeds raw bytes (decoded as streaming UTF-8). */
  pushBytes(chunk: Uint8Array): void {
    this.push(this.decoder.decode(chunk, { stream: true }))
  }

  /** Feeds decoded text. */
  push(text: string): void {
    if (!this.started && text.length > 0) {
      this.started = true
      if (text.charCodeAt(0) === 0xfeff) text = text.slice(1)
    }
    let start = 0
    for (let i = 0; i < text.length; i++) {
      const c = text.charCodeAt(i)
      if (c === 0x0a) {
        // "\n" right after a "\r" (possibly from the previous chunk) is part of CRLF.
        if (this.pendingCR && i === start && this.line === '') {
          this.pendingCR = false
          start = i + 1
          continue
        }
        this.endLine(text.slice(start, i))
        start = i + 1
      } else if (c === 0x0d) {
        this.endLine(text.slice(start, i))
        start = i + 1
        this.pendingCR = true
        continue
      }
      this.pendingCR = false
    }
    if (start < text.length) {
      this.pendingCR = false
      this.line += text.slice(start)
      if (this.line.length > this.maxEventChars) throw new SseLimitError(this.maxEventChars)
    }
  }

  /**
   * Ends the stream: flushes the decoder. A trailing event without its
   * terminating blank line is discarded (per the specification), so a
   * truncated stream never yields a partial event.
   */
  end(): void {
    const rest = this.decoder.decode()
    if (rest) this.push(rest)
    this.line = ''
    this.data = []
    this.dataChars = 0
    this.eventName = ''
  }

  private endLine(part: string): void {
    const line = this.line + part
    this.line = ''
    if (line === '') {
      this.dispatch()
      return
    }
    if (line.charCodeAt(0) === 0x3a) return // ":" comment (heartbeat)
    const colon = line.indexOf(':')
    let field = line
    let value = ''
    if (colon >= 0) {
      field = line.slice(0, colon)
      value = line.slice(colon + 1)
      if (value.charCodeAt(0) === 0x20) value = value.slice(1)
    }
    switch (field) {
      case 'event':
        this.eventName = value
        break
      case 'data':
        this.dataChars += value.length + 1
        if (this.dataChars > this.maxEventChars) throw new SseLimitError(this.maxEventChars)
        this.data.push(value)
        break
      case 'id':
        if (!value.includes('\0')) this.lastId = value
        break
      default:
        // "retry" and unknown fields: not used (the stream is not resumable).
        break
    }
  }

  private dispatch(): void {
    const name = this.eventName
    const data = this.data
    this.eventName = ''
    this.data = []
    this.dataChars = 0
    if (data.length === 0) return
    this.onMessage({ event: name === '' ? 'message' : name, data: data.join('\n'), id: this.lastId })
  }
}
