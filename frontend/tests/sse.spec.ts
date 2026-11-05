import { describe, expect, it } from 'vitest'
import { SseLimitError, SseParser, type SseMessage } from '@/api/sse'

function parseChunks(chunks: string[]): SseMessage[] {
  const out: SseMessage[] = []
  const p = new SseParser((m) => out.push(m))
  for (const c of chunks) p.push(c)
  p.end()
  return out
}

const STREAM =
  'event: meta\ndata: {"a":1}\n\n' +
  ': keep-alive\n\n' +
  'event: delta\r\ndata: {"text":"ş🎉"}\r\n\r\n' +
  'event: multi\rdata: line1\rdata: line2\r\r' +
  'data: no name\n\n' +
  'event: validated\ndata: {"done":true}\n\n'

const EXPECTED = [
  { event: 'meta', data: '{"a":1}', id: '' },
  { event: 'delta', data: '{"text":"ş🎉"}', id: '' },
  { event: 'multi', data: 'line1\nline2', id: '' },
  { event: 'message', data: 'no name', id: '' },
  { event: 'validated', data: '{"done":true}', id: '' },
]

describe('SseParser', () => {
  it('parses LF, CRLF and CR line endings, comments, multi-line data and default names', () => {
    expect(parseChunks([STREAM])).toEqual(EXPECTED)
  })

  it('handles every split position of the text (incl. between \\r and \\n)', () => {
    for (let i = 0; i <= STREAM.length; i++) {
      expect(parseChunks([STREAM.slice(0, i), STREAM.slice(i)])).toEqual(EXPECTED)
    }
    expect(parseChunks([...STREAM])).toEqual(EXPECTED) // one character at a time
  })

  it('decodes UTF-8 split inside multi-byte sequences', () => {
    const bytes = new TextEncoder().encode(STREAM)
    for (const size of [1, 2, 3, 5, 7]) {
      const out: SseMessage[] = []
      const p = new SseParser((m) => out.push(m))
      for (let i = 0; i < bytes.length; i += size) p.pushBytes(bytes.slice(i, i + size))
      p.end()
      expect(out).toEqual(EXPECTED)
    }
  })

  it('ignores a leading BOM, unknown fields, retry and events without data', () => {
    const out = parseChunks(['﻿retry: 3000\nfoo: bar\nevent: x\n\nid: 7\nevent: y\ndata:  spaced\n\n'])
    expect(out).toEqual([{ event: 'y', data: ' spaced', id: '7' }])
  })

  it('discards a trailing event without its blank line (truncated stream)', () => {
    expect(parseChunks(['event: meta\ndata: {}\n\nevent: validated\ndata: {"x":1}'])).toEqual([
      { event: 'meta', data: '{}', id: '' },
    ])
    expect(parseChunks(['event: validated\ndata: {"x":1}\n'])).toEqual([])
  })

  it('bounds memory per line and per event', () => {
    const p = new SseParser(() => {}, 100)
    expect(() => p.push('data: ' + 'x'.repeat(200))).toThrow(SseLimitError)
    const q = new SseParser(() => {}, 100)
    expect(() => {
      for (let i = 0; i < 20; i++) q.push('data: 0123456789\n')
    }).toThrow(SseLimitError)
  })
})
