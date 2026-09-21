import { describe, expect, it } from 'vitest'
import { HOURS_PER_YEAR, estimateFees, liquidityForDeposit, poolLiquidity, type FeeEstimateInput } from '@/lib/feeEstimate'

/** A pool priced in dollars, so a dollar deposited is a dollar of quote token. */
const base = (over: Partial<FeeEstimateInput> = {}): FeeEstimateInput => ({
  price: 100,
  min: 95,
  max: 105,
  depositUsd: 1,
  quoteUsd: 1,
  activeLiquidity: 1e24,
  decimals0: 18,
  decimals1: 18,
  volume1hUsd: 50_000,
  feeBps: 30,
  ...over,
})

describe('estimateFees', () => {
  it('pays the fee rate on the hour’s volume, in proportion to the share held', () => {
    // A pool small enough that a dollar is exactly half of it: 1 unit of L
    // already there, and a range priced so the dollar buys 1 unit too.
    const price = 1
    const mine = liquidityForDeposit(1, price, 0.9, 1.1)!
    const e = estimateFees(base({ price, min: 0.9, max: 1.1, activeLiquidity: mine * 1e18, volume1hUsd: 1000, feeBps: 100 }))!
    expect(e.share).toBeCloseTo(0.5, 6)
    expect(e.feeUsd1h).toBeCloseTo(1000 * 0.01 * 0.5, 6) // $5
  })

  it('earns more from a narrower range, because the same money buys more liquidity', () => {
    const narrow = estimateFees(base({ min: 99, max: 101 }))!
    const wide = estimateFees(base({ min: 50, max: 200 }))!
    expect(narrow.feeUsd1h).toBeGreaterThan(wide.feeUsd1h)
    expect(narrow.share).toBeGreaterThan(wide.share)
  })

  it('earns nothing when the price sits outside the range', () => {
    const e = estimateFees(base({ min: 110, max: 120 }))!
    expect(e.inRange).toBe(false)
    expect(e.feeUsd1h).toBe(0)
    expect(e.aprPct).toBe(0)
  })

  it('carries the hour out to a year against the deposit', () => {
    const e = estimateFees(base())!
    expect(e.aprPct).toBeCloseTo((e.feeUsd1h / 1) * HOURS_PER_YEAR * 100, 6)
  })

  it('scales the deposit by the quote token’s dollar price', () => {
    const dollars = estimateFees(base({ quoteUsd: 1 }))!
    const ether = estimateFees(base({ quoteUsd: 2000 }))!
    // A dollar buys a two-thousandth as much of a token worth $2,000, so it
    // buys a two-thousandth of the liquidity and earns about that share.
    expect(dollars.share / ether.share).toBeCloseTo(2000, 0)
  })

  it('handles a full range, where one bound is zero and the other unbounded', () => {
    const e = estimateFees(base({ min: 0, max: Infinity }))!
    expect(e.inRange).toBe(true)
    expect(e.feeUsd1h).toBeGreaterThan(0)
  })

  it('refuses to guess when a figure it needs is missing', () => {
    expect(estimateFees(base({ volume1hUsd: null }))).toBeNull()
    expect(estimateFees(base({ feeBps: null }))).toBeNull()
    expect(estimateFees(base({ quoteUsd: null }))).toBeNull()
    expect(estimateFees(base({ activeLiquidity: 0 }))).toBeNull()
    expect(estimateFees(base({ depositUsd: 0 }))).toBeNull()
    expect(estimateFees(base({ min: 105, max: 95 }))).toBeNull()
  })
})

describe('the pool’s own units', () => {
  // Read live from Onion / USDC on Arc, a Uniswap v3 pool with one full-range
  // position: token0 is USDC (6 decimals), token1 is Onion (18), and the
  // backend reported both the raw L and what that L holds. Valuing the pool's
  // own liquidity with the maths used to size a deposit has to come back to
  // the same number, or a deposit's share of it would be off by the decimals.
  const raw = 1302504264530229800
  const price = 0.0000017283452536273517
  const bin = { lower: 0.000001696517359141616, upper: 3.3781585789443593e50, usdc: 15.839999988909376, onion: 990749594.4607348 }

  it('values a pool’s reported liquidity as the tokens the pool says it holds', () => {
    const pool = poolLiquidity(raw, 6, 18)
    // liquidityForDeposit(1, …) is 1 / (quote per unit of L), so dividing by it
    // turns L back into quote tokens.
    const perUnit = 1 / liquidityForDeposit(1, price, bin.lower, bin.upper)!
    const valueInUsdc = pool * perUnit
    expect(valueInUsdc).toBeCloseTo(bin.usdc + bin.onion * price, 2)
  })
})
