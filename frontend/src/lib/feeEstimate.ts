/**
 * What a deposit would earn in fees over the next hour, between two prices.
 *
 * A concentrated position is paid the pool's fee rate on the volume that
 * crosses it, in proportion to its share of the liquidity active where the
 * price is. So the estimate itself is one line — volume x fee rate x share —
 * and all the work is in the share: turning a dollar amount into L, the unit
 * the pool states its own liquidity in, and comparing the two.
 *
 * The conversion is the standard position maths. A position of liquidity L
 * over [a, b] holds, at price p inside that range,
 *
 *     base  = L (1/√p - 1/√b)      quote = L (√p - √a)
 *
 * so it is worth `L (2√p - p/√b - √a)` in quote tokens. Reading that backwards
 * gives the L a deposit buys, and the narrower the range the more L the same
 * money buys — which is the whole reason to pick one.
 *
 * The pool reports L in raw units, scaled by both tokens' decimals; dividing by
 * 10^((d0+d1)/2) puts it in the same units as the L above, which is stated in
 * whole tokens at displayed prices. That factor is symmetric in the two tokens,
 * so it holds whichever way round the pair is shown.
 *
 * This is a forecast of one hour built from the last one. It assumes the next
 * hour trades like the one behind it and that the price stays in the range; a
 * pair whose volume is fading, or whose price walks out of the range, earns
 * less. It says nothing about impermanent loss, which is the other half of what
 * a position is worth.
 */

/** Hours in a year, for carrying an hourly figure out to an annual rate. */
export const HOURS_PER_YEAR = 24 * 365

export interface FeeEstimateInput {
  /** Current price, quote tokens per traded token. */
  price: number
  /** The chosen range. `min` may be 0 and `max` Infinity for a full range. */
  min: number
  max: number
  /** What is being deposited, in dollars. */
  depositUsd: number
  /** Dollar price of the quote token; null when the backend cannot price it. */
  quoteUsd: number | null
  /** The pool's own L at the current price, raw, as the pool reports it. */
  activeLiquidity: number
  decimals0: number
  decimals1: number
  volume1hUsd: number | null
  feeBps: number | null
}

export interface FeeEstimate {
  /** False when the current price sits outside the range: it earns nothing. */
  inRange: boolean
  /** The deposit's share of the liquidity active at the current price, 0..1. */
  share: number
  /** Fees over the next hour, in dollars. */
  feeUsd1h: number
  /** That hour carried out over a year, as a percentage of the deposit. */
  aprPct: number
  /** The volume the estimate is built on, in dollars. */
  volume1hUsd: number
}

/**
 * The liquidity a deposit buys over [min, max], in whole-token units — the
 * same units `poolLiquidity` converts into. Null when the range cannot hold a
 * position at this price.
 */
export function liquidityForDeposit(depositQuote: number, price: number, min: number, max: number): number | null {
  if (!(price > 0) || !(depositQuote > 0) || !(max > min)) return null
  const sp = Math.sqrt(price)
  const sa = Math.sqrt(Math.max(min, 0))
  const sb = Math.sqrt(max)
  // Only the part of the range the price is actually inside is paid for: above
  // the range the deposit is all quote, below it all base.
  const perL = 2 * sp - (Number.isFinite(sb) ? price / sb : 0) - sa
  if (!(perL > 0)) return null
  return depositQuote / perL
}

/** The pool's raw L in whole-token units, so a deposit can be compared with it. */
export function poolLiquidity(raw: number, decimals0: number, decimals1: number): number {
  return raw / 10 ** ((decimals0 + decimals1) / 2)
}

/** Null when something the estimate needs is missing, rather than a guessed zero. */
export function estimateFees(i: FeeEstimateInput): FeeEstimate | null {
  const { price, min, max, depositUsd, quoteUsd, activeLiquidity, volume1hUsd, feeBps } = i
  if (!(price > 0) || !(depositUsd > 0)) return null
  if (quoteUsd === null || !(quoteUsd > 0)) return null
  if (volume1hUsd === null || volume1hUsd < 0 || feeBps === null || !(feeBps > 0)) return null
  if (!(activeLiquidity > 0) || !(max > min)) return null

  if (!(price > min && price < max)) {
    return { inRange: false, share: 0, feeUsd1h: 0, aprPct: 0, volume1hUsd }
  }

  const mine = liquidityForDeposit(depositUsd / quoteUsd, price, min, max)
  if (mine === null) return null
  const pool = poolLiquidity(activeLiquidity, i.decimals0, i.decimals1)
  if (!(pool > 0)) return null

  // Adding to the pool grows it, so the share is of the liquidity that will be
  // there once the deposit is in — not of the pool as it stands.
  const share = mine / (mine + pool)
  const feeUsd1h = volume1hUsd * (feeBps / 10_000) * share
  return {
    inRange: true,
    share,
    feeUsd1h,
    aprPct: (feeUsd1h / depositUsd) * HOURS_PER_YEAR * 100,
    volume1hUsd,
  }
}
