import { describe, expect, it } from 'vitest'
import { dexscreenerUrl, NATIVE_ADDRESS, orientPool } from '@/lib/chains'
import type { ChainInfo } from '@/types/api'
import { makePool } from './fixtures'

const rbn: ChainInfo = {
  id: 'rbn', name: 'Robinhood', tag: 'RBN', kind: 'evm', head: 0, ingesting: true,
  stables: ['0x5fc5360d0400a0fd4f2af552add042d716f1d168'], wrappedNative: '0x0bd7d308f8e1639fab988df18a8011f41eacad73',
  dexes: ['Uniswap v4'],
}

describe('orientPool', () => {
  const eth = { address: NATIVE_ADDRESS, symbol: 'ETH', decimals: 18 }
  const usdg = { address: '0x5fc5360D0400a0Fd4f2af552ADD042D716F1d168', symbol: 'USDG', decimals: 6 }
  const meme = { address: '0x3ba6f3a9d3c80debc90f440e2a8eef14e92b1e18', symbol: 'LONGCOLN', decimals: 18 }

  it('trades the token, not native ETH sitting in slot 0 of a v4 pair', () => {
    const o = orientPool(makePool({ pair: 'LONGCOLN / ETH', token0: eth, token1: meme }), rbn)
    expect(o.base?.symbol).toBe('LONGCOLN')
    expect(o.quote?.symbol).toBe('ETH')
  })

  it('prices in the stable over the native currency', () => {
    const o = orientPool(makePool({ pair: 'ETH / USDG', token0: usdg, token1: eth }), rbn)
    expect(o.base?.symbol).toBe('ETH')
    expect(o.quote?.symbol).toBe('USDG')
  })

  it('falls back to the pair name before chain details load', () => {
    const o = orientPool(makePool({ pair: 'LONGCOLN / ETH', token0: eth, token1: meme }), null)
    expect(o.base?.symbol).toBe('LONGCOLN')
  })
})

describe('dexscreenerUrl', () => {
  it('links a Robinhood pool (a v4 PoolId) straight to its DexScreener page', () => {
    const id = '0x42b961e421c08741fe9b25dda1d1090d42c7f782a0d73e099b4248d67357a612'
    expect(dexscreenerUrl('rbn', id, 'TSLA / FLAP')).toBe(`https://dexscreener.com/robinhood/${id}`)
  })

  it('links every indexed EVM chain by pool, never by a pair-name search', () => {
    for (const chain of ['eth', 'base', 'bsc', 'arb', 'rbn']) {
      expect(dexscreenerUrl(chain, '0xabc', 'A / B')).not.toContain('/search')
    }
  })

  it('falls back to a search for a chain DexScreener has no slug for', () => {
    expect(dexscreenerUrl('unknown', '0xabc', 'A / B')).toBe('https://dexscreener.com/search?q=A%2FB')
  })
})
