/**
 * Concentrated-liquidity range maths for the Liquidity tab. Pure functions so
 * the draggable band, the steppers and the readouts all agree, and so the
 * behaviour is unit-testable without a DOM.
 */
import { price as fmtPrice } from './format'

export type PresetId = 'tight' | 'mid' | 'wide' | 'full'

export interface RangePreset {
  id: PresetId
  name: string
  pct: number | null
}

export const RANGE_PRESETS: RangePreset[] = [
  { id: 'tight', name: '±2%', pct: 0.02 },
  { id: 'mid', name: '±5%', pct: 0.05 },
  { id: 'wide', name: '±15%', pct: 0.15 },
  { id: 'full', name: 'Full range', pct: null },
]

export const FEE_TIERS_BPS = [1, 5, 30, 100] as const

export interface RangeInput {
  price: number
  preset: PresetId
  /** User-edited bounds as typed; null means "derive from the preset". */
  minText: string | null
  maxText: string | null
}

export interface RangeView {
  lo: number
  span: number
}

export interface RangeState {
  min: number
  max: number
  minText: string
  maxText: string
  lowBounded: boolean
  highBounded: boolean
  valid: boolean
  inRange: boolean
  /** Largest one-sided distance from price, as a fraction; null for full range. */
  spread: number | null
  efficiency: number | null
  widthLabel: string
  status: 'In range now' | 'Out of range' | 'Always in range' | 'Invalid range'
}

export function presetOf(id: PresetId): RangePreset {
  return RANGE_PRESETS.find((p) => p.id === id) ?? RANGE_PRESETS[1]!
}

function parseBound(text: string | null, fallback: number): number {
  if (text === null) return fallback
  const t = text.trim()
  if (t === '∞' || t.toLowerCase() === 'inf') return Infinity
  const v = parseFloat(t.replace(/,/g, ''))
  return Number.isFinite(v) ? v : NaN
}

export function computeRange({ price, preset, minText, maxText }: RangeInput): RangeState {
  const p = presetOf(preset)
  const derivedMin = p.pct === null ? 0 : price * (1 - p.pct)
  const derivedMax = p.pct === null ? Infinity : price * (1 + p.pct)
  const rawMin = parseBound(minText, derivedMin)
  const rawMax = parseBound(maxText, derivedMax)
  const min = Number.isFinite(rawMin) ? Math.max(0, rawMin) : rawMin === Infinity ? Infinity : NaN
  const max = rawMax

  const lowBounded = Number.isFinite(min) && min > 0
  const highBounded = Number.isFinite(max)
  const valid = !Number.isNaN(min) && !Number.isNaN(max) && max > min
  const inRange = valid && price >= min && price <= max

  const spreads: number[] = []
  if (lowBounded) spreads.push((price - min) / price)
  if (highBounded) spreads.push((max - price) / price)
  const spread = spreads.length ? Math.max(...spreads.map(Math.abs)) : null
  const efficiency = !valid ? null : spread === null ? 1 : Math.max(1, Math.round(1 / Math.max(spread, 0.002)))

  const widthLabel = !valid
    ? '—'
    : spread === null
      ? 'full range'
      : (lowBounded && highBounded ? '±' : lowBounded ? '-' : '+') + (spread * 100).toFixed(1) + '%'

  const status: RangeState['status'] = !valid
    ? 'Invalid range'
    : spread === null
      ? 'Always in range'
      : inRange
        ? 'In range now'
        : 'Out of range'

  return {
    min,
    max,
    minText: minText ?? (p.pct === null ? '0' : fmtPrice(derivedMin)),
    maxText: maxText ?? (p.pct === null ? '∞' : fmtPrice(derivedMax)),
    lowBounded,
    highBounded,
    valid,
    inRange,
    spread,
    efficiency,
    widthLabel,
    status,
  }
}

/** The axis viewport: wide enough to show both bounds with some air. */
export function axisView(price: number, r: RangeState): RangeView {
  const reach =
    Math.max(
      r.lowBounded ? price - Math.min(r.min, price) : 0,
      r.highBounded ? r.max - price : 0,
      price * 0.02,
    ) * 1.35
  const lo = Math.max(0, price - reach)
  return { lo, span: price + reach - lo }
}

export function toPct(v: number, view: RangeView): number {
  if (view.span <= 0) return 0
  return Math.max(0, Math.min(100, ((v - view.lo) / view.span) * 100))
}

export interface BandGeometry {
  left: number
  width: number
  priceLeft: number
}

export function bandGeometry(price: number, r: RangeState, view: RangeView): BandGeometry {
  let left = 0
  let width = 100
  if (r.valid) {
    left = r.lowBounded ? toPct(r.min, view) : 0
    const right = r.highBounded ? toPct(r.max, view) : 100
    width = Math.max(1.5, Math.min(100 - left, right - left))
  } else {
    width = 0
  }
  return { left, width, priceLeft: toPct(price, view) }
}

/**
 * Value under the pointer, clamped so min stays strictly below max. `other` is
 * the opposite bound (Infinity/0 when unbounded).
 */
export function dragValue(which: 'min' | 'max', frac: number, view: RangeView, other: number, price: number): number {
  const f = Math.max(0, Math.min(1, frac))
  let v = Math.max(0, view.lo + f * view.span)
  if (which === 'min') {
    const cap = Number.isFinite(other) ? other : price * 4
    v = Math.min(v, cap * 0.995)
  } else {
    const floor = Number.isFinite(other) && other > 0 ? other : 0
    v = Math.max(v, floor * 1.005, Number.EPSILON)
  }
  return v
}

/** ▲▼ steppers move a bound by ±1 %. */
export function step(text: string, dir: 1 | -1, price: number): string {
  const t = text.trim()
  let v = t === '∞' ? price * 1.5 : parseFloat(t.replace(/,/g, ''))
  if (!Number.isFinite(v) || v <= 0) v = price
  return fmtPrice(Math.max(0, v * (dir === 1 ? 1.01 : 0.99)))
}
