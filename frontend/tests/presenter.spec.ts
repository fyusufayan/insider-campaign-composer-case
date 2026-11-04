import { describe, expect, it } from 'vitest'
import type { SegmentRule } from '@/api/types'
import { cityName, foldCityKey, CITY_COUNT } from '@/segment/cities'
import { describeCondition, presentRule, ruleOneLiner } from '@/segment/presenter'

describe('city catalog', () => {
  it('mirrors the 81-province backend catalog', () => {
    expect(CITY_COUNT).toBe(81)
  })
  it.each([
    ['istanbul', 'İstanbul'],
    ['izmir', 'İzmir'],
    ['igdir', 'Iğdır'],
    ['isparta', 'Isparta'],
    ['sanliurfa', 'Şanlıurfa'],
    ['canakkale', 'Çanakkale'],
    ['ISTANBUL', 'İstanbul'],
  ])('resolves normalized key %s → %s', (key, name) => {
    expect(cityName(key)).toBe(name)
  })
  it('folds like segment.FoldKey', () => {
    expect(foldCityKey('  İSTANBUL ')).toBe('istanbul')
    expect(foldCityKey('Kahraman  Maraş')).toBe('kahraman maras')
    expect(foldCityKey('İstanbul')).toBe('istanbul') // decomposed İ
  })
  it('returns null for an unknown key (never guesses)', () => {
    expect(cityName('atlantis')).toBeNull()
  })
})

describe('describeCondition: attribute', () => {
  const cases: Array<[unknown, string]> = [
    [{ type: 'attribute', field: 'city', op: 'eq', value: 'istanbul' }, 'City is İstanbul'],
    [{ type: 'attribute', field: 'city', op: 'neq', value: 'ankara' }, 'City is not Ankara'],
    [{ type: 'attribute', field: 'city', op: 'in', value: ['ankara', 'izmir'] }, 'City is one of Ankara, İzmir'],
    [{ type: 'attribute', field: 'city', op: 'not_in', value: ['ankara', 'izmir'] }, 'City is none of Ankara, İzmir'],
    [{ type: 'attribute', field: 'city', op: 'in', value: ['bursa'] }, 'City is Bursa'],
    [{ type: 'attribute', field: 'city', op: 'eq', value: 'atlantis' }, 'City is “atlantis”'],
    [{ type: 'attribute', field: 'country', op: 'eq', value: 'TR' }, 'Country is Türkiye (TR)'],
    [{ type: 'attribute', field: 'country', op: 'in', value: ['DE', 'TR'] }, 'Country is one of Germany (DE), Türkiye (TR)'],
    [{ type: 'attribute', field: 'gender', op: 'eq', value: 'female' }, 'Gender is female'],
    [{ type: 'attribute', field: 'gender', op: 'in', value: ['female', 'other'] }, 'Gender is one of female, other'],
    [{ type: 'attribute', field: 'age', op: 'gte', value: 18 }, 'Age is at least 18'],
    [{ type: 'attribute', field: 'age', op: 'lte', value: 65 }, 'Age is at most 65'],
    [{ type: 'attribute', field: 'age', op: 'between', value: { min: 25, max: 40 } }, 'Age is between 25 and 40'],
    [{ type: 'attribute', field: 'signup_days_ago', op: 'lte', value: 14 }, 'Signed up in the last 14 days'],
    [{ type: 'attribute', field: 'signup_days_ago', op: 'gte', value: 365 }, 'Signed up at least 365 days ago'],
    [{ type: 'attribute', field: 'signup_days_ago', op: 'between', value: { min: 7, max: 30 } }, 'Signed up between 7 and 30 days ago'],
    [{ type: 'attribute', field: 'last_active_days_ago', op: 'gte', value: 90 }, 'Last active at least 90 days ago'],
    [{ type: 'attribute', field: 'last_active_days_ago', op: 'lte', value: 1 }, 'Last active in the last day'],
    [{ type: 'attribute', field: 'last_active_days_ago', op: 'lte', value: 0 }, 'Last active today'],
    [{ type: 'attribute', field: 'language', op: 'eq', value: 'tr' }, 'Language is Turkish (tr)'],
    [{ type: 'attribute', field: 'language', op: 'in', value: ['en', 'tr'] }, 'Language is one of English (en), Turkish (tr)'],
  ]
  it.each(cases)('%j', (cond, want) => {
    const v = describeCondition(cond)
    expect(v.known).toBe(true)
    expect(v.kind).toBe('attribute')
    expect(v.text).toBe(want)
  })
})

describe('describeCondition: event and behavior', () => {
  const cases: Array<[unknown, string]> = [
    [{ type: 'event', event: 'cart_add', op: 'exists', within_days: 30 }, 'Added to cart in the last 30 days'],
    [{ type: 'event', event: 'cart_add', op: 'not_exists', within_days: 7 }, 'No cart additions in the last 7 days'],
    [{ type: 'event', event: 'purchase', op: 'exists', within_days: 1 }, 'Purchased in the last day'],
    [{ type: 'event', event: 'purchase', op: 'not_exists', within_days: 60 }, 'No purchases in the last 60 days'],
    [
      { type: 'event', event: 'purchase', op: 'count_gte', count: 2, within_days: 90, category_in: ['electronics'] },
      'Purchased at least 2 times in the last 90 days (category: electronics)',
    ],
    [
      { type: 'event', event: 'cart_add', op: 'count_lte', count: 1, within_days: 30, category_in: ['books', 'toys'] },
      'Added to cart at most 1 time in the last 30 days (categories: books, toys)',
    ],
    [{ type: 'behavior', predicate: 'abandoned_cart', within_days: 30 }, 'Abandoned cart in the last 30 days'],
  ]
  it.each(cases)('%j', (cond, want) => {
    const v = describeCondition(cond)
    expect(v.known).toBe(true)
    expect(v.text).toBe(want)
  })

  it('explains abandoned_cart precisely', () => {
    expect(describeCondition({ type: 'behavior', predicate: 'abandoned_cart', within_days: 10 }).hint).toMatch(
      /has not purchased since/,
    )
  })
})

describe('describeCondition: unrecognized shapes are never paraphrased', () => {
  it.each([
    [null],
    ['city=istanbul'],
    [{ type: 'attribute', field: 'email_consent', op: 'eq', value: 'opted_in' }],
    [{ type: 'attribute', field: 'city', op: 'eq', value: ['istanbul'] }],
    [{ type: 'attribute', field: 'age', op: 'between', value: { min: 1 } }],
    [{ type: 'event', event: 'cart_add', op: 'exists', count: 2, within_days: 5 }],
    [{ type: 'event', event: 'cart_add', op: 'count_gte', within_days: 5 }],
    [{ type: 'event', event: 'view', op: 'exists', within_days: 5 }],
    [{ type: 'behavior', predicate: 'churn_risk', within_days: 5 }],
    [{ type: 'sql', value: 'DROP TABLE users' }],
  ])('%j', (cond) => {
    const v = describeCondition(cond)
    expect(v.known).toBe(false)
    expect(v.kind).toBe('unknown')
    expect(v.text).toMatch(/Unrecognized condition/)
  })
})

describe('presentRule', () => {
  it('renders the example rule as the acceptance chips', () => {
    const rule: SegmentRule = {
      version: 1,
      match: 'all',
      conditions: [
        { type: 'attribute', field: 'city', op: 'eq', value: 'istanbul' },
        { type: 'behavior', predicate: 'abandoned_cart', within_days: 30 },
      ],
    }
    const view = presentRule(rule)
    expect(view.matchLabel).toBe('Users must match all of the following')
    expect(ruleOneLiner(view)).toBe('City is İstanbul · Abandoned cart in the last 30 days')
    expect(view.conditionCount).toBe(2)
    expect(view.hasUnknown).toBe(false)
  })

  it('renders top-level any with shallow groups', () => {
    const rule: SegmentRule = {
      version: 1,
      match: 'any',
      conditions: [{ type: 'attribute', field: 'language', op: 'eq', value: 'en' }],
      groups: [
        {
          match: 'all',
          conditions: [
            { type: 'attribute', field: 'city', op: 'in', value: ['ankara', 'izmir'] },
            { type: 'attribute', field: 'gender', op: 'eq', value: 'female' },
          ],
        },
        {
          match: 'any',
          conditions: [
            { type: 'event', event: 'purchase', op: 'not_exists', within_days: 60 },
            { type: 'behavior', predicate: 'abandoned_cart', within_days: 7 },
          ],
        },
      ],
    }
    const view = presentRule(rule)
    expect(view.matchLabel).toBe('Users must match any of the following')
    expect(view.groups).toHaveLength(2)
    expect(view.groups[0]!.matchLabel).toBe('Matches all of')
    expect(view.groups[1]!.matchLabel).toBe('Matches any of')
    expect(view.conditionCount).toBe(5)
    expect(ruleOneLiner(view)).toBe(
      'Language is English (en) or (City is one of Ankara, İzmir and Gender is female) or (No purchases in the last 60 days or Abandoned cart in the last 7 days)',
    )
  })

  it('flags unknown conditions inside groups', () => {
    const rule = {
      match: 'all',
      conditions: [],
      groups: [{ match: 'all', conditions: [{ type: 'mystery' }] }],
    } as unknown as SegmentRule
    expect(presentRule(rule).hasUnknown).toBe(true)
  })
})
