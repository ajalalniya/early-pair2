/**
 * Display formatting, implemented once. Every function takes possibly-missing
 * input and renders an em dash for it: a blank is honest, a zero is a claim.
 */
import { formatUnits } from 'viem'

export const DASH = '—'

type Maybe<T> = T | null | undefined

const isNum = (n: Maybe<number>): n is number => typeof n === 'number' && Number.isFinite(n)

/** `$1.24M`, `$412.0K`, `$842`. */
export function money(n: Maybe<number>): string {
  if (!isNum(n)) return DASH
  const sign = n < 0 ? '-' : ''
  const a = Math.abs(n)
  if (a >= 1e6) return `${sign}$${(a / 1e6).toFixed(2)}M`
  if (a >= 1e3) return `${sign}$${(a / 1e3).toFixed(1)}K`
  return `${sign}$${Math.round(a).toLocaleString('en-US')}`
}

/**
 * Money to three decimals: `$1.24M`, `$412.0K`, `$8.421`, `$0.012`.
 *
 * The portfolio shows what a wallet actually holds, and a position worth a
 * couple of cents is a real position — rounded to whole dollars it reads as
 * `$0`, which looks like a bug or a loss. Feed figures stay on `money`: there,
 * whole dollars are what a glance wants. Trailing zeros beyond two decimals are
 * dropped, so ordinary amounts still read as `$6.25`, and an amount too small
 * for three decimals keeps three significant digits instead of collapsing.
 */
export function moneyFine(n: Maybe<number>): string {
  if (!isNum(n)) return DASH
  const sign = n < 0 ? '-' : ''
  const a = Math.abs(n)
  if (a >= 1e6) return `${sign}$${(a / 1e6).toFixed(2)}M`
  if (a >= 1e3) return `${sign}$${(a / 1e3).toFixed(1)}K`
  if (a > 0 && a < 0.001) return `${sign}$${Number(a.toPrecision(3))}`
  const [int = '0', frac = ''] = a.toFixed(3).split('.')
  const trimmed = frac.replace(/0+$/, '').padEnd(2, '0')
  return `${sign}$${Number(int).toLocaleString('en-US')}.${trimmed}`
}

/** Signed money for P&L and fees: `+$214`, `-$1.2K`. */
export function signedMoney(n: Maybe<number>): string {
  if (!isNum(n)) return DASH
  return (n >= 0 ? '+' : '-') + money(Math.abs(n)).slice(1)
}

/** Signed money to three decimals, for the portfolio's P&L figures. */
export function signedMoneyFine(n: Maybe<number>): string {
  if (!isNum(n)) return DASH
  return (n >= 0 ? '+' : '-') + moneyFine(Math.abs(n)).slice(1)
}

export function num(n: Maybe<number>): string {
  if (!isNum(n)) return DASH
  return n.toLocaleString('en-US')
}

export function price(v: Maybe<number>): string {
  if (!isNum(v)) return DASH
  return v < 0.01 ? Number(v.toPrecision(3)).toString() : v.toFixed(4)
}

export function pct(v: Maybe<number>): string {
  if (!isNum(v)) return DASH
  return (v > 0 ? '+' : '') + v.toFixed(1) + '%'
}

/** Basis points as a plain percentage: `42 -> "0.42 %"`. */
export function bps(v: Maybe<number>): string {
  if (!isNum(v)) return DASH
  return (v / 100).toFixed(2) + ' %'
}

/** `0x7a4f…2Be1`: six leading characters, four trailing, case preserved. */
export function addr(a: Maybe<string>): string {
  if (!a) return DASH
  if (a.length <= 12) return a
  return `${a.slice(0, 6)}…${a.slice(-4)}`
}

/** Tx hashes read better a little longer: `0x9c41…7ae2`. */
export const hash = addr

/** Age from a timestamp, recomputed by the caller on a 1s ticker. */
export function age(ts: Maybe<string | number | Date>, now: number = Date.now()): string {
  if (ts === null || ts === undefined) return DASH
  const t = ts instanceof Date ? ts.getTime() : typeof ts === 'number' ? ts : Date.parse(ts)
  if (!Number.isFinite(t)) return DASH
  const s = Math.max(0, Math.floor((now - t) / 1000))
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h`
  return `${Math.floor(h / 24)}d`
}

export function ageMinutes(ts: Maybe<string>, now: number = Date.now()): number | null {
  if (!ts) return null
  const t = Date.parse(ts)
  if (!Number.isFinite(t)) return null
  return Math.max(0, (now - t) / 60000)
}

/** Uptime in `6h 12m` form. */
export function duration(sec: Maybe<number>): string {
  if (!isNum(sec)) return DASH
  const s = Math.max(0, Math.floor(sec))
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  if (d > 0) return `${d}d ${h}h`
  if (h > 0) return `${h}h ${String(m).padStart(2, '0')}m`
  if (m > 0) return `${m}m`
  return `${s}s`
}

/**
 * A base-unit decimal string rendered with token decimals. Uses viem's
 * bigint formatting so wei values never pass through a float.
 */
export function units(raw: Maybe<string>, decimals: Maybe<number>, maxFraction = 4): string {
  if (!raw || !isNum(decimals)) return DASH
  let value: bigint
  try {
    value = BigInt(raw)
  } catch {
    return DASH
  }
  return trimDecimal(formatUnits(value, decimals), maxFraction)
}

/** Group a plain decimal string and cap its fraction without float rounding. */
export function trimDecimal(dec: string, maxFraction = 4): string {
  const neg = dec.startsWith('-')
  const body = neg ? dec.slice(1) : dec
  const [intPart = '0', frac = ''] = body.split('.')
  const grouped = intPart.replace(/\B(?=(\d{3})+(?!\d))/g, ',')
  let f = frac.slice(0, maxFraction).replace(/0+$/, '')
  // Keep tiny amounts visible rather than rounding them to zero.
  if (!f && intPart === '0' && /[1-9]/.test(frac)) {
    const firstSig = frac.search(/[1-9]/)
    f = frac.slice(0, firstSig + 3).replace(/0+$/, '')
  }
  return (neg ? '-' : '') + grouped + (f ? '.' + f : '')
}

/** Ratio line used in the trade panel and fraud card. */
export function ratio(buyers: Maybe<number>, sellers: Maybe<number>): string {
  if (!isNum(buyers) || !isNum(sellers)) return DASH
  if (sellers === 0) return buyers > 0 ? '∞ : 1' : DASH
  return (buyers / sellers).toFixed(1) + ' : 1'
}

export function buyerSellerRatio(buyers: Maybe<number>, sellers: Maybe<number>): number | null {
  if (!isNum(buyers) || !isNum(sellers)) return null
  if (sellers === 0) return buyers > 0 ? Infinity : null
  return buyers / sellers
}
