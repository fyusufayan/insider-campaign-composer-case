// Read-only presenter for normalized segment rules (DSL v1, ARCHITECTURE §6.1).
//
// Every sentence is derived mechanically from the stored, validated rule —
// never from the model's free-text summary — so the "active conditions" the
// marketer sees are exactly what the backend compiles to SQL. A condition
// whose shape is not recognized is reported as unrecognized (with its JSON)
// instead of being paraphrased.

import type { Match, SegmentRule } from '@/api/types'
import { cityName } from './cities'

export type ConditionKind = 'attribute' | 'event' | 'behavior' | 'unknown'

export interface ConditionView {
  kind: ConditionKind
  /** Human-readable description, e.g. "City is İstanbul". */
  text: string
  /** Optional precise definition (shown as secondary text). */
  hint?: string
  /** false when the shape was not recognized; text is then a placeholder. */
  known: boolean
  /** The condition exactly as stored (for the diagnostic JSON view). */
  raw: unknown
}

export interface GroupView {
  match: Match
  matchLabel: string
  conditions: ConditionView[]
}

export interface RuleView {
  match: Match
  /** e.g. "Users must match all of the following". */
  matchLabel: string
  conditions: ConditionView[]
  groups: GroupView[]
  /** Number of conditions incl. those inside groups. */
  conditionCount: number
  hasUnknown: boolean
}

type Obj = Record<string, unknown>

function isObj(v: unknown): v is Obj {
  return typeof v === 'object' && v !== null && !Array.isArray(v)
}

function isInt(v: unknown): v is number {
  return typeof v === 'number' && Number.isInteger(v)
}

function isStrArray(v: unknown): v is string[] {
  return Array.isArray(v) && v.length > 0 && v.every((x) => typeof x === 'string')
}

function isRange(v: unknown): v is { min: number; max: number } {
  return isObj(v) && isInt(v.min) && isInt(v.max) && Object.keys(v).length === 2
}

export function plural(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`
}

export function lastDays(n: number): string {
  return n === 1 ? 'in the last day' : `in the last ${n} days`
}

function joinList(items: string[]): string {
  return items.join(', ')
}

const LANGUAGE_NAMES: Record<string, string> = { tr: 'Turkish', en: 'English' }

function countryName(code: string): string {
  try {
    const dn = new Intl.DisplayNames(['en'], { type: 'region' })
    const name = dn.of(code)
    if (name && name !== code) return `${name} (${code})`
  } catch {
    // unknown region code or no Intl support: show the code
  }
  return code
}

function cityLabel(key: string): string {
  return cityName(key) ?? `“${key}”`
}

/** Display form of one string value of the given field. */
function valueLabel(field: string, v: string): string {
  switch (field) {
    case 'city':
      return cityLabel(v)
    case 'country':
      return countryName(v)
    case 'language':
      return LANGUAGE_NAMES[v] ? `${LANGUAGE_NAMES[v]} (${v})` : v
    default:
      return v
  }
}

const STRING_FIELD_LABEL: Record<string, string> = {
  city: 'City',
  country: 'Country',
  gender: 'Gender',
  language: 'Language',
}

function describeAttribute(c: Obj): Pick<ConditionView, 'text' | 'hint'> | null {
  const { field, op, value } = c
  if (typeof field !== 'string' || typeof op !== 'string') return null

  const label = STRING_FIELD_LABEL[field]
  if (label) {
    if ((op === 'eq' || op === 'neq') && typeof value === 'string') {
      return { text: `${label} is ${op === 'neq' ? 'not ' : ''}${valueLabel(field, value)}` }
    }
    if ((op === 'in' || op === 'not_in') && isStrArray(value)) {
      const list = joinList(value.map((v) => valueLabel(field, v)))
      if (value.length === 1) {
        return { text: `${label} is ${op === 'not_in' ? 'not ' : ''}${list}` }
      }
      return { text: `${label} is ${op === 'not_in' ? 'none of' : 'one of'} ${list}` }
    }
    return null
  }

  if (field === 'age') {
    if (op === 'gte' && isInt(value)) return { text: `Age is at least ${value}` }
    if (op === 'lte' && isInt(value)) return { text: `Age is at most ${value}` }
    if (op === 'between' && isRange(value)) return { text: `Age is between ${value.min} and ${value.max}` }
    return null
  }

  if (field === 'signup_days_ago' || field === 'last_active_days_ago') {
    const verb = field === 'signup_days_ago' ? 'Signed up' : 'Last active'
    if (op === 'gte' && isInt(value)) return { text: `${verb} at least ${plural(value, 'day', 'days')} ago` }
    if (op === 'lte' && isInt(value)) {
      return value === 0
        ? { text: `${verb} today` }
        : { text: `${verb} ${lastDays(value)}`, hint: `at most ${plural(value, 'day', 'days')} ago` }
    }
    if (op === 'between' && isRange(value) && field === 'signup_days_ago') {
      return { text: `${verb} between ${value.min} and ${value.max} days ago` }
    }
    return null
  }

  return null
}

function categorySuffix(c: Obj): string | null {
  if (c.category_in === undefined) return ''
  if (!isStrArray(c.category_in)) return null
  return ` (${c.category_in.length === 1 ? 'category' : 'categories'}: ${joinList(c.category_in)})`
}

function describeEvent(c: Obj): Pick<ConditionView, 'text' | 'hint'> | null {
  const { event, op, count, within_days: days } = c
  if (event !== 'cart_add' && event !== 'purchase') return null
  if (!isInt(days)) return null
  const cats = categorySuffix(c)
  if (cats === null) return null
  const window = lastDays(days)
  const isCart = event === 'cart_add'

  switch (op) {
    case 'exists':
      if (count !== undefined) return null
      return { text: `${isCart ? 'Added to cart' : 'Purchased'} ${window}${cats}` }
    case 'not_exists':
      if (count !== undefined) return null
      return { text: `${isCart ? 'No cart additions' : 'No purchases'} ${window}${cats}` }
    case 'count_gte':
    case 'count_lte': {
      if (!isInt(count)) return null
      const bound = op === 'count_gte' ? 'at least' : 'at most'
      const times = plural(count, 'time', 'times')
      return {
        text: `${isCart ? 'Added to cart' : 'Purchased'} ${bound} ${times} ${window}${cats}`,
        hint: op === 'count_lte' ? 'users with no such events count as 0 and match' : undefined,
      }
    }
    default:
      return null
  }
}

function describeBehavior(c: Obj): Pick<ConditionView, 'text' | 'hint'> | null {
  if (c.predicate !== 'abandoned_cart' || !isInt(c.within_days)) return null
  return {
    text: `Abandoned cart ${lastDays(c.within_days)}`,
    hint: 'added a product to the cart in the window and has not purchased since',
  }
}

export function describeCondition(raw: unknown): ConditionView {
  const unknown: ConditionView = {
    kind: 'unknown',
    text: 'Unrecognized condition (see the rule JSON)',
    known: false,
    raw,
  }
  if (!isObj(raw)) return unknown
  let d: Pick<ConditionView, 'text' | 'hint'> | null = null
  let kind: ConditionKind = 'unknown'
  switch (raw.type) {
    case 'attribute':
      d = describeAttribute(raw)
      kind = 'attribute'
      break
    case 'event':
      d = describeEvent(raw)
      kind = 'event'
      break
    case 'behavior':
      d = describeBehavior(raw)
      kind = 'behavior'
      break
  }
  if (!d) return unknown
  const view: ConditionView = { kind, text: d.text, known: true, raw }
  if (d.hint) view.hint = d.hint
  return view
}

export function matchLabel(match: Match, scope: 'rule' | 'group'): string {
  const which = match === 'all' ? 'all' : 'any'
  return scope === 'rule' ? `Users must match ${which} of the following` : `Matches ${which} of`
}

export function presentRule(rule: SegmentRule): RuleView {
  const conditions = rule.conditions.map(describeCondition)
  const groups: GroupView[] = (rule.groups ?? []).map((g) => ({
    match: g.match,
    matchLabel: matchLabel(g.match, 'group'),
    conditions: g.conditions.map(describeCondition),
  }))
  const all = [...conditions, ...groups.flatMap((g) => g.conditions)]
  return {
    match: rule.match,
    matchLabel: matchLabel(rule.match, 'rule'),
    conditions,
    groups,
    conditionCount: all.length,
    hasUnknown: all.some((c) => !c.known),
  }
}

/** Single-line summary of a rule, e.g. "City is İstanbul · Abandoned cart in the last 30 days". */
export function ruleOneLiner(view: RuleView): string {
  const parts = [
    ...view.conditions.map((c) => c.text),
    ...view.groups.map((g) => `(${g.conditions.map((c) => c.text).join(g.match === 'all' ? ' and ' : ' or ')})`),
  ]
  return parts.join(view.match === 'all' ? ' · ' : ' or ')
}
