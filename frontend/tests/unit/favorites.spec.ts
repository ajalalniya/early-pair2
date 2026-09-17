import { QueryClient } from '@tanstack/vue-query'
import { describe, expect, it } from 'vitest'
import { addFavorite, applyFavoriteChange, dropFavorite, favoriteKeys, patchFavorite } from '@/queries/favorites'
import { qk } from '@/queries/keys'
import type { Pool } from '@/types/api'
import { PoolStateUpdateSchema } from '@/types/api'
import { makePool } from './fixtures'

const a = makePool({ address: '0x' + '1'.repeat(40), pair: 'AAA / WETH' })
const b = makePool({ address: '0x' + '2'.repeat(40), pair: 'BBB / WETH' })

describe('favorites cache helpers', () => {
  it('puts a newly starred pair on top and replaces one already listed in place', () => {
    expect(addFavorite([a], b).map((p) => p.pair)).toEqual(['BBB / WETH', 'AAA / WETH'])
    const updated = addFavorite([a, b], { ...b, liquidityUsd: 7 })
    expect(updated.map((p) => p.pair)).toEqual(['AAA / WETH', 'BBB / WETH'])
    expect(updated[1]!.liquidityUsd).toBe(7)
  })

  it('drops an unstarred pair, matching addresses case-insensitively', () => {
    expect(dropFavorite([a, b], a.chainId, a.address.toUpperCase().replace('0X', '0x'))).toEqual([b])
  })

  it('patches figures of a listed favorite and ignores pools that are not starred', () => {
    const list = [a]
    expect(patchFavorite(list, { chainId: a.chainId, address: a.address, volume1hUsd: 42 })![0]!.volume1hUsd).toBe(42)
    expect(patchFavorite(list, { chainId: b.chainId, address: b.address, volume1hUsd: 42 })).toBe(list)
  })

  it('keys favorites the way feed rows are keyed', () => {
    expect(favoriteKeys([a]).has(`${a.chainId}:${a.address.toLowerCase()}`)).toBe(true)
  })

  it('applies a star from another tab using the pool the tab already has, and an unstar', () => {
    const qc = new QueryClient()
    qc.setQueryData<Pool[]>(qk.favorites, [])
    qc.setQueryData<Pool>(qk.pool(b.chainId, b.address), b)

    applyFavoriteChange(qc, b.chainId, b.address, '2026-01-01T12:00:00Z')
    const listed = qc.getQueryData<Pool[]>(qk.favorites)!
    expect(listed.map((p) => p.pair)).toEqual(['BBB / WETH'])
    expect(listed[0]!.favoritedAt).toBe('2026-01-01T12:00:00Z')

    applyFavoriteChange(qc, b.chainId, b.address, null)
    expect(qc.getQueryData<Pool[]>(qk.favorites)).toEqual([])
  })
})

describe('partial pools.update frames', () => {
  it('accepts a removal, a star and an unstar, and rejects a frame that changes nothing', () => {
    const base = { chainId: 'base', address: a.address }
    expect(PoolStateUpdateSchema.safeParse({ ...base, removed: true, removedAt: '2026-01-01T12:00:00Z' }).success).toBe(true)
    expect(PoolStateUpdateSchema.safeParse({ ...base, favoritedAt: '2026-01-01T12:00:00Z' }).success).toBe(true)
    expect(PoolStateUpdateSchema.safeParse({ ...base, favoritedAt: null }).success).toBe(true)
    expect(PoolStateUpdateSchema.safeParse(base).success).toBe(false)
  })
})
