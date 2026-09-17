import { describe, expect, it } from 'vitest'
import { flattenFeed, insertPools, patchPool, removePool, type FeedData } from '@/queries/feed'
import { makePool } from './fixtures'

const a = makePool({ address: '0x' + '1'.repeat(40), createdAt: '2026-01-01T12:00:00Z' })
const b = makePool({ address: '0x' + '2'.repeat(40), createdAt: '2026-01-01T11:00:00Z' })
const feed = (): FeedData => ({ pages: [{ data: [a, b], nextCursor: 'x' }], pageParams: [null] })

describe('feed cache helpers', () => {
  it('inserts new pools at the top, newest first, without duplicates', () => {
    const c = makePool({ address: '0x' + '3'.repeat(40), createdAt: '2026-01-01T12:05:00Z' })
    const d = makePool({ address: '0x' + '4'.repeat(40), createdAt: '2026-01-01T12:10:00Z' })
    const r = insertPools(feed(), [c, d, c])
    expect(flattenFeed(r.data).map((p) => p.address)).toEqual([d.address, c.address, a.address, b.address])
    expect(r.inserted).toHaveLength(2)
  })

  it('replaces a pool that is already present instead of duplicating it', () => {
    const updated = { ...a, liquidityUsd: 1 }
    const r = insertPools(feed(), [updated])
    expect(r.inserted).toHaveLength(0)
    expect(flattenFeed(r.data)).toHaveLength(2)
    expect(flattenFeed(r.data)[0]!.liquidityUsd).toBe(1)
  })

  it('does not insert a pool that arrives already reorged', () => {
    const r = insertPools(feed(), [makePool({ address: '0x' + '9'.repeat(40), state: 'reorged' })])
    expect(r.inserted).toHaveLength(0)
  })

  it('matches addresses case-insensitively when patching', () => {
    const next = patchPool(feed(), { chainId: a.chainId, address: a.address.toUpperCase().replace('0X', '0x'), state: 'confirmed' })
    expect(flattenFeed(next)[0]!.state).toBe('confirmed')
  })

  it('merges a state-only update without dropping market data', () => {
    const next = patchPool(feed(), { chainId: b.chainId, address: b.address, state: 'unconfirmed' })
    const row = flattenFeed(next)[1]!
    expect(row.state).toBe('unconfirmed')
    expect(row.liquidityUsd).toBe(b.liquidityUsd)
  })

  it('removes a pool', () => {
    expect(flattenFeed(removePool(feed(), a.chainId, a.address))).toEqual([b])
  })
})
