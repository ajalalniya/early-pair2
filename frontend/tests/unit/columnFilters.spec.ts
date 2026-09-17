import { describe, expect, it } from 'vitest'
import {
  decodeRange,
  describeRange,
  emptyFilters,
  encodeRange,
  filtersFromQuery,
  matchesColumns,
  parseBound,
} from '@/lib/columnFilters'
import { makePool } from './fixtures'

describe('parseBound', () => {
  it('reads money and counts with k/m/b, $ and commas', () => {
    expect(parseBound('20k', 'usd')).toBe(20_000)
    expect(parseBound('$1.5M', 'usd')).toBe(1_500_000)
    expect(parseBound('20,000', 'usd')).toBe(20_000)
    expect(parseBound('', 'usd')).toBeNull()
    expect(parseBound('lots', 'usd')).toBeNaN()
  })

  it('reads signed percentages and ages with units', () => {
    expect(parseBound('-20', 'pct')).toBe(-20)
    expect(parseBound('+50%', 'pct')).toBe(50)
    expect(parseBound('30', 'minutes')).toBe(30)
    expect(parseBound('2h', 'minutes')).toBe(120)
    expect(parseBound('1d', 'minutes')).toBe(1440)
  })
})

describe('matchesColumns', () => {
  const now = Date.parse('2026-01-01T12:10:00Z')
  const pool = makePool({ createdAt: '2026-01-01T12:00:00Z', liquidityUsd: 25_000, volume1hUsd: null, buyers1h: 12, sellers1h: 3, priceChange1h: -30 })

  it('keeps a pool inside every range', () => {
    const f = emptyFilters()
    f.liq = { min: 20_000, max: null }
    f.age = { min: null, max: 30 }
    f.buy = { min: 10, max: null }
    f.chg = { min: -50, max: 0 }
    expect(matchesColumns(pool, f, now)).toBe(true)
  })

  it('drops a pool outside a range', () => {
    const f = emptyFilters()
    f.liq = { min: 50_000, max: null }
    expect(matchesColumns(pool, f, now)).toBe(false)
    const g = emptyFilters()
    g.age = { min: null, max: 5 } // ten minutes old
    expect(matchesColumns(pool, g, now)).toBe(false)
  })

  it('does not let an unknown value pass a set bound', () => {
    const f = emptyFilters()
    f.vol = { min: 0, max: null }
    expect(matchesColumns(pool, f, now)).toBe(false)
    expect(matchesColumns(pool, emptyFilters(), now)).toBe(true)
  })
})

describe('URL and labels', () => {
  it('round-trips ranges, negative bounds included', () => {
    expect(encodeRange({ min: 20_000, max: null })).toBe('20000~')
    expect(decodeRange('-50~200')).toEqual({ min: -50, max: 200 })
    expect(decodeRange('~30')).toEqual({ min: null, max: 30 })
    expect(decodeRange('garbage')).toEqual({ min: null, max: null })
    expect(filtersFromQuery({ liq: '20000~', chain: 'base' }).liq).toEqual({ min: 20_000, max: null })
  })

  it('describes ranges the way the chip shows them', () => {
    expect(describeRange({ min: 20_000, max: null }, 'usd')).toBe('≥ $20K')
    expect(describeRange({ min: null, max: 60 }, 'minutes')).toBe('≤ 1h')
    expect(describeRange({ min: -20, max: 50 }, 'pct')).toBe('-20%–+50%')
  })
})
