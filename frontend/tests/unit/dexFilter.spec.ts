import { describe, expect, it } from 'vitest'
import { ALL_DEXES, dexFromQuery, dexOptions, dexSlug, matchesDex } from '@/lib/dexFilter'
import type { ChainInfo, Pool } from '@/types/api'

const chain = (id: string, dexes: string[]) => ({ id, name: id, tag: id, kind: 'evm', head: 0, ingesting: true, stables: [], dexes }) as unknown as ChainInfo

const pool = (dex: string) => ({ dex }) as Pool

describe('dexSlug', () => {
  it('makes a URL-safe slug that survives spaces and case', () => {
    expect(dexSlug('Uniswap v3')).toBe('uniswap-v3')
    expect(dexSlug('PancakeSwap v2')).toBe('pancakeswap-v2')
    expect(dexSlug('  Curve  ')).toBe('curve')
  })
})

describe('dexOptions', () => {
  const chains = [chain('eth', ['Uniswap v2', 'Uniswap v3']), chain('arc', ['Uniswap v4', 'Uniswap v3'])]

  it('lists every DEX once across chains, in order', () => {
    expect(dexOptions(chains, ALL_DEXES)).toEqual(['Uniswap v2', 'Uniswap v3', 'Uniswap v4'])
  })

  // A chip for a DEX the selected chain has no factory for could only ever
  // produce an empty feed.
  it('narrows to the selected chain', () => {
    expect(dexOptions(chains, 'arc')).toEqual(['Uniswap v4', 'Uniswap v3'])
  })

  it('copes with a backend that reports no chains yet', () => {
    expect(dexOptions(undefined, ALL_DEXES)).toEqual([])
  })
})

describe('matchesDex', () => {
  it('matches by slug, and "all" matches everything', () => {
    expect(matchesDex(pool('Uniswap v3'), 'uniswap-v3')).toBe(true)
    expect(matchesDex(pool('Uniswap v4'), 'uniswap-v3')).toBe(false)
    expect(matchesDex(pool('Uniswap v4'), ALL_DEXES)).toBe(true)
  })
})

describe('dexFromQuery', () => {
  const options = ['Uniswap v3', 'Uniswap v4']

  it('keeps a selection the listed DEXes still offer', () => {
    expect(dexFromQuery('uniswap-v3', options)).toBe('uniswap-v3')
  })

  // Switching chains can leave a slug behind that nothing on the new chain
  // matches; keeping it would empty the feed with no visible cause.
  it('drops a selection no listed DEX matches', () => {
    expect(dexFromQuery('pancakeswap-v3', options)).toBe(ALL_DEXES)
    expect(dexFromQuery(undefined, options)).toBe(ALL_DEXES)
    expect(dexFromQuery('', options)).toBe(ALL_DEXES)
  })
})
