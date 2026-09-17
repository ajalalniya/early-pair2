import { describe, expect, it } from 'vitest'
import { band, checkRows, isGated, matchesRisk, verdict } from '@/lib/risk'
import { makePool } from './fixtures'

describe('band', () => {
  it('uses the design thresholds', () => {
    expect(band(0).label).toBe('SAFE')
    expect(band(24).label).toBe('SAFE')
    expect(band(25).label).toBe('WATCH')
    expect(band(59).label).toBe('WATCH')
    expect(band(60).label).toBe('DANGER')
    expect(band(100).label).toBe('DANGER')
  })
})

describe('gate', () => {
  it('blocks at or above the threshold only when on', () => {
    expect(isGated(60, true, 60)).toBe(true)
    expect(isGated(59, true, 60)).toBe(false)
    expect(isGated(91, false, 60)).toBe(false)
    expect(isGated(null, true, 60)).toBe(false)
  })
})

describe('matchesRisk', () => {
  it('excludes unscored pools from band filters, like the backend', () => {
    const unscored = makePool({ risk: null })
    expect(matchesRisk(unscored, 'all')).toBe(true)
    expect(matchesRisk(unscored, 'safe')).toBe(false)
  })
})

describe('verdict', () => {
  it('leads with the honeypot pattern', () => {
    const p = makePool({ buyers1h: 1214, sellers1h: 3, risk: { score: 91, band: 'DANGER', verdict: '', checks: [], complete: true } })
    expect(verdict(p, 60)).toBe('1,214 addresses bought, only 3 ever sold — honeypot pattern. Trading is gated.')
  })
  it('then the threshold, then mixed, then clean', () => {
    const risk = (score: number) => ({ score, band: '', verdict: '', checks: [], complete: true })
    expect(verdict(makePool({ risk: risk(70) }), 60)).toMatch(/^High probability/)
    expect(verdict(makePool({ risk: risk(40) }), 60)).toMatch(/^Mixed signals/)
    expect(verdict(makePool({ risk: risk(10) }), 60)).toMatch(/^No critical failures/)
  })
})

describe('checkRows', () => {
  it('always shows the eight rows in order, unknown when not checked', () => {
    const p = makePool({
      risk: {
        score: 10,
        band: 'SAFE',
        verdict: '',
        complete: false,
        checks: [
          { label: 'LP lock', value: 'Locked 12mo', ok: true, known: true },
          { label: 'Holders', value: '12', ok: false, known: true },
        ],
      },
    })
    const rows = checkRows(p)
    expect(rows.map((r) => r.label)).toEqual([
      'Unique sellers',
      'Buy / sell address ratio',
      'LP lock',
      'Mint authority',
      'Buy / sell tax',
      'Top 10 holders',
      'Holders',
      'Contract verified',
    ])
    expect(rows[2]).toMatchObject({ state: 'pass', value: 'Locked 12mo' })
    expect(rows[6]).toMatchObject({ state: 'fail' })
    expect(rows[0]).toMatchObject({ state: 'unknown' })
  })
})
