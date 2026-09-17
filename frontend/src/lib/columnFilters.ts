/**
 * Range filters on the pool table's columns: age, liquidity, 1h volume, buyers,
 * sellers and 1h price change.
 *
 * A bound that is set excludes rows whose value is not known yet: a pair whose
 * liquidity has not been read cannot be shown to be above $20K, so it is not
 * listed as if it were. Filters live in the URL, like chain and risk, as
 * `min~max` with either side optional (`liq=20000~`, `chg=-50~200`).
 */
import type { Pool } from '@/types/api'
import { ageMinutes } from './format'

export interface Range {
  min: number | null
  max: number | null
}

export type ColumnKey = 'age' | 'liq' | 'vol' | 'buy' | 'sell' | 'chg'

export type ColumnFilters = Record<ColumnKey, Range>

/** How a column's numbers are typed and shown. */
export type RangeKind = 'minutes' | 'usd' | 'count' | 'pct'

export const COLUMN_KIND: Record<ColumnKey, RangeKind> = {
  age: 'minutes',
  liq: 'usd',
  vol: 'usd',
  buy: 'count',
  sell: 'count',
  chg: 'pct',
}

export const COLUMN_KEYS = Object.keys(COLUMN_KIND) as ColumnKey[]

const open = (): Range => ({ min: null, max: null })

export function emptyFilters(): ColumnFilters {
  return { age: open(), liq: open(), vol: open(), buy: open(), sell: open(), chg: open() }
}

export const isSet = (r: Range) => r.min !== null || r.max !== null

export function activeCount(f: ColumnFilters): number {
  return COLUMN_KEYS.filter((k) => isSet(f[k])).length
}

const SUFFIX: Record<string, number> = { k: 1e3, m: 1e6, b: 1e9 }
const AGE_UNIT: Record<string, number> = { s: 1 / 60, m: 1, min: 1, h: 60, d: 1440 }

/**
 * Parse what someone types into a bound: `20k`, `$1.5M`, `20,000` for money and
 * counts; `-20` or `+50%` for change; `30`, `30m`, `2h`, `1d` for age (minutes
 * when no unit). Empty means no bound; anything unreadable is NaN.
 */
export function parseBound(text: string, kind: RangeKind): number | null {
  const t = text.trim().toLowerCase().replace(/[$,_\s]/g, '')
  if (t === '') return null
  if (kind === 'minutes') {
    const m = t.match(/^(\d*\.?\d+)(s|min|m|h|d)?$/)
    return m ? parseFloat(m[1]!) * (AGE_UNIT[m[2] ?? 'm'] ?? 1) : NaN
  }
  if (kind === 'pct') {
    const m = t.match(/^([+-]?\d*\.?\d+)%?$/)
    return m ? parseFloat(m[1]!) : NaN
  }
  const m = t.match(/^(\d*\.?\d+)([kmb])?$/)
  return m ? parseFloat(m[1]!) * (m[2] ? SUFFIX[m[2]]! : 1) : NaN
}

function inRange(v: number | null, r: Range): boolean {
  if (!isSet(r)) return true
  if (v === null || !Number.isFinite(v)) return false
  if (r.min !== null && v < r.min) return false
  if (r.max !== null && v > r.max) return false
  return true
}

export function matchesColumns(p: Pool, f: ColumnFilters, now: number): boolean {
  return (
    inRange(ageMinutes(p.createdAt, now), f.age) &&
    inRange(p.liquidityUsd, f.liq) &&
    inRange(p.volume1hUsd, f.vol) &&
    inRange(p.buyers1h, f.buy) &&
    inRange(p.sellers1h, f.sell) &&
    inRange(p.priceChange1h, f.chg)
  )
}

// ------------------------------------------------------------------ URL

export function encodeRange(r: Range): string | undefined {
  if (!isSet(r)) return undefined
  return `${r.min ?? ''}~${r.max ?? ''}`
}

export function decodeRange(v: unknown): Range {
  if (typeof v !== 'string' || !v.includes('~')) return open()
  const [lo = '', hi = ''] = v.split('~')
  const num = (s: string) => (s !== '' && Number.isFinite(Number(s)) ? Number(s) : null)
  return { min: num(lo), max: num(hi) }
}

export function filtersFromQuery(q: Record<string, unknown>): ColumnFilters {
  const f = emptyFilters()
  for (const k of COLUMN_KEYS) f[k] = decodeRange(q[k])
  return f
}

// ------------------------------------------------------------------ labels

function compact(n: number): string {
  const a = Math.abs(n)
  const sign = n < 0 ? '-' : ''
  const trim = (x: number) => String(parseFloat(x.toFixed(2)))
  if (a >= 1e9) return `${sign}${trim(a / 1e9)}B`
  if (a >= 1e6) return `${sign}${trim(a / 1e6)}M`
  if (a >= 1e3) return `${sign}${trim(a / 1e3)}K`
  return `${sign}${trim(a)}`
}

/** A bound as the chip shows it and the input is refilled with. */
export function formatBound(n: number, kind: RangeKind): string {
  switch (kind) {
    case 'usd':
      return `$${compact(n)}`
    case 'count':
      return compact(n)
    case 'pct':
      return `${n > 0 ? '+' : ''}${parseFloat(n.toFixed(2))}%`
    case 'minutes':
      if (n >= 1440 && n % 1440 === 0) return `${n / 1440}d`
      if (n >= 60 && n % 60 === 0) return `${n / 60}h`
      return `${parseFloat(n.toFixed(2))}m`
  }
}

/** "≥ $20K", "≤ 30m", "$20K–$100K". */
export function describeRange(r: Range, kind: RangeKind): string {
  if (r.min !== null && r.max !== null) return `${formatBound(r.min, kind)}–${formatBound(r.max, kind)}`
  if (r.min !== null) return `≥ ${formatBound(r.min, kind)}`
  if (r.max !== null) return `≤ ${formatBound(r.max, kind)}`
  return ''
}
