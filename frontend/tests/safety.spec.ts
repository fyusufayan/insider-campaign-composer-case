// Static guard: no raw-HTML rendering anywhere in the application source.
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'

function files(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const p = join(dir, name)
    return statSync(p).isDirectory() ? files(p) : [p]
  })
}

describe('no unsafe HTML rendering', () => {
  const src = join(__dirname, '..', 'src')
  const all = files(src).filter((f) => /\.(vue|ts)$/.test(f))

  it('scans the source tree', () => {
    expect(all.length).toBeGreaterThan(10)
  })

  it.each(['v-html', 'innerHTML', 'outerHTML', 'insertAdjacentHTML', 'document.write'])('never uses %s', (needle) => {
    const offenders = all.filter((f) => readFileSync(f, 'utf8').includes(needle))
    expect(offenders).toEqual([])
  })
})
