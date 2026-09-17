import type { Pool } from '@/types/api'
import { buyerSellerRatio, num } from './format'

export type RiskFilter = 'all' | 'safe' | 'watch' | 'danger'
export type BandId = 'safe' | 'watch' | 'danger'

export interface Band {
  id: BandId
  label: 'SAFE' | 'WATCH' | 'DANGER'
  fg: string
  bg: string
}

const BANDS: Record<BandId, Band> = {
  safe: { id: 'safe', label: 'SAFE', fg: 'var(--risk-safe-fg)', bg: 'var(--risk-safe-bg)' },
  watch: { id: 'watch', label: 'WATCH', fg: 'var(--risk-watch-fg)', bg: 'var(--risk-watch-bg)' },
  danger: { id: 'danger', label: 'DANGER', fg: 'var(--risk-danger-fg)', bg: 'var(--risk-danger-bg)' },
}

export function band(score: number): Band {
  if (score < 25) return BANDS.safe
  if (score < 60) return BANDS.watch
  return BANDS.danger
}

export function matchesRisk(pool: Pool, filter: RiskFilter): boolean {
  if (filter === 'all') return true
  // The backend excludes unscored pools from any band filter; mirror that so a
  // live insert does not show up under a filter a refetch would drop it from.
  if (!pool.risk) return false
  return band(pool.risk.score).id === filter
}

export function isGated(score: number | null | undefined, gateOn: boolean, threshold: number): boolean {
  return gateOn && typeof score === 'number' && score >= threshold
}

/** Plain-language verdict, in the priority order the design specifies. */
export function verdict(pool: Pool, threshold: number): string {
  const r = buyerSellerRatio(pool.buyers1h, pool.sellers1h)
  if (r !== null && r > 20) {
    return `${num(pool.buyers1h)} addresses bought, only ${num(pool.sellers1h)} ever sold — honeypot pattern. Trading is gated.`
  }
  if (!pool.risk) return 'This pool has not been scored yet. Treat it as unverified.'
  const score = pool.risk.score
  if (score >= threshold) return 'High probability of rug or honeypot. Trading is gated.'
  if (score >= 25) return 'Mixed signals — seller base is thin relative to buyers. Size positions carefully.'
  return 'No critical failures detected. Buyer and seller address counts are balanced.'
}

/** The eight rows, in design order, filled from the scorer's checks by label. */
export const CHECK_LABELS = [
  'Unique sellers',
  'Buy / sell address ratio',
  'LP lock',
  'Mint authority',
  'Buy / sell tax',
  'Top 10 holders',
  'Holders',
  'Contract verified',
] as const

export interface CheckRow {
  label: string
  value: string
  state: 'pass' | 'fail' | 'unknown'
}

export function checkRows(pool: Pool): CheckRow[] {
  const found = new Map((pool.risk?.checks ?? []).map((c) => [c.label, c]))
  const rows: CheckRow[] = CHECK_LABELS.map((label) => {
    const c = found.get(label)
    if (!c || !c.known) return { label, value: c?.value || 'not checked yet', state: 'unknown' }
    return { label, value: c.value, state: c.ok ? 'pass' : 'fail' }
  })
  // Anything extra the scorer reports is still shown, after the fixed eight.
  for (const c of pool.risk?.checks ?? []) {
    if (!(CHECK_LABELS as readonly string[]).includes(c.label)) {
      rows.push({ label: c.label, value: c.value, state: !c.known ? 'unknown' : c.ok ? 'pass' : 'fail' })
    }
  }
  return rows
}
