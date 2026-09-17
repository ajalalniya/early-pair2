import { describe, expect, it } from 'vitest'
import { DASH, addr, age, bps, duration, money, moneyFine, num, pct, price, ratio, signedMoney, signedMoneyFine, trimDecimal, units } from '@/lib/format'

describe('money', () => {
  it('follows the design rule', () => {
    expect(money(1_240_000)).toBe('$1.24M')
    expect(money(412_000)).toBe('$412.0K')
    expect(money(842)).toBe('$842')
    expect(money(0)).toBe('$0')
  })
  it('renders a dash for missing values instead of a guess', () => {
    expect(money(null)).toBe('—')
    expect(money(undefined)).toBe('—')
    expect(money(Number.NaN)).toBe('—')
  })
  it('signs gains and losses the way the design does, without the dollar sign', () => {
    expect(signedMoney(214)).toBe('+214')
    expect(signedMoney(-1200)).toBe('-1.2K')
  })
})

describe('num / price / pct / bps', () => {
  it('formats', () => {
    expect(num(1214)).toBe('1,214')
    expect(price(0.00418)).toBe('0.00418')
    expect(price(1.48)).toBe('1.4800')
    expect(pct(64.2)).toBe('+64.2%')
    expect(pct(-12.6)).toBe('-12.6%')
    expect(pct(0)).toBe('0.0%')
    expect(bps(42)).toBe('0.42 %')
  })
})

describe('addr', () => {
  it('keeps six leading and four trailing characters with case intact', () => {
    expect(addr('0x7a4fC0ffee00000000000000000000000002Be1')).toBe('0x7a4f…2Be1')
  })
})

describe('age', () => {
  const now = Date.parse('2026-01-01T12:00:00Z')
  it('is computed from the timestamp', () => {
    expect(age('2026-01-01T11:58:00Z', now)).toBe('2m')
    expect(age('2026-01-01T11:19:00Z', now)).toBe('41m')
    expect(age('2026-01-01T09:00:00Z', now)).toBe('3h')
    expect(age('2025-12-31T11:00:00Z', now)).toBe('1d')
    expect(age('2026-01-01T11:59:48Z', now)).toBe('12s')
    expect(age('not a date', now)).toBe('—')
  })
})

describe('units', () => {
  it('formats wei without going through a float', () => {
    expect(units('1234567890123456789012', 18)).toBe('1,234.5678')
    expect(units('5000000', 6)).toBe('5')
    // A value too large for a double stays exact in its integer part.
    expect(units('123456789012345678901234567890', 18)).toBe('123,456,789,012.3456')
  })
  it('keeps dust visible', () => {
    expect(units('1234', 18)).toBe('0.00000000000000123')
  })
  it('rejects junk', () => {
    expect(units('abc', 18)).toBe('—')
    expect(units('1', undefined)).toBe('—')
  })
  it('trimDecimal groups and caps', () => {
    expect(trimDecimal('6800.000000', 2)).toBe('6,800')
    expect(trimDecimal('84.2', 4)).toBe('84.2')
  })
})

describe('ratio', () => {
  it('handles zero sellers', () => {
    expect(ratio(1214, 3)).toBe('404.7 : 1')
    expect(ratio(5, 0)).toBe('∞ : 1')
    expect(ratio(null, 3)).toBe('—')
  })
})

describe('duration', () => {
  it('formats uptime', () => {
    expect(duration(6 * 3600 + 12 * 60)).toBe('6h 12m')
    expect(duration(18 * 60)).toBe('18m')
    expect(duration(null)).toBe('—')
  })
})

describe('moneyFine', () => {
  // The portfolio's own formatter: a two-cent position is a real position, and
  // `money` would round it to $0, which reads as a bug or a total loss.
  it('keeps small amounts visible to three decimals', () => {
    expect(moneyFine(0.012)).toBe('$0.012')
    expect(moneyFine(0.4)).toBe('$0.40')
    expect(moneyFine(0)).toBe('$0.00')
  })

  it('reads normally for ordinary amounts', () => {
    expect(moneyFine(6.25)).toBe('$6.25')
    expect(moneyFine(124.5)).toBe('$124.50')
    expect(moneyFine(1)).toBe('$1.00')
  })

  it('still compacts thousands and millions', () => {
    expect(moneyFine(412_000)).toBe('$412.0K')
    expect(moneyFine(1_240_000)).toBe('$1.24M')
  })

  it('keeps significant digits for amounts below a tenth of a cent', () => {
    expect(moneyFine(0.000123)).toBe('$0.000123')
  })

  it('signs and dashes like the others', () => {
    expect(moneyFine(-0.012)).toBe('-$0.012')
    // Signed figures drop the currency mark, as signedMoney already does.
    expect(signedMoneyFine(0.012)).toBe('+0.012')
    expect(signedMoneyFine(-2.5)).toBe('-2.50')
    expect(moneyFine(null)).toBe(DASH)
    expect(signedMoneyFine(undefined)).toBe(DASH)
  })
})
