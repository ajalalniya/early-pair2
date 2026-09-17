import { describe, expect, it } from 'vitest'
import { BACKOFF_MAX_MS, BACKOFF_MIN_MS, backoffDelay } from '@/composables/useEventStream'
import { summarize } from '@/queries'
import { FrameSchema, PoolSchema, PoolStateUpdateSchema, WatchHitDataSchema } from '@/types/api'
import { parseTopicInput } from '@/lib/topic'

describe('backoff', () => {
  it('starts at half a second and caps at fifteen', () => {
    expect(backoffDelay(0, () => 1)).toBe(BACKOFF_MIN_MS)
    expect(backoffDelay(0, () => 0)).toBe(BACKOFF_MIN_MS / 2)
    expect(backoffDelay(3, () => 1)).toBe(4000)
    expect(backoffDelay(20, () => 1)).toBe(BACKOFF_MAX_MS)
    expect(backoffDelay(20, () => 0)).toBe(BACKOFF_MAX_MS / 2)
  })
})

describe('frame validation', () => {
  it('accepts the documented envelope', () => {
    expect(FrameSchema.safeParse({ t: 'pools.new', seq: 918233, ts: 1750000000123, d: {} }).success).toBe(true)
  })

  it('rejects malformed frames rather than throwing', () => {
    expect(FrameSchema.safeParse({ seq: 'nope' }).success).toBe(false)
    expect(PoolSchema.safeParse({ chainId: 'eth', address: 42 }).success).toBe(false)
    expect(WatchHitDataSchema.safeParse({ kind: 'bogus' }).success).toBe(false)
  })

  it('tells a state-only update apart from a full pool', () => {
    const partial = { chainId: 'base', address: '0xabc', state: 'reorged' }
    expect(PoolSchema.safeParse(partial).success).toBe(false)
    expect(PoolStateUpdateSchema.safeParse(partial).success).toBe(true)
  })

  it('keeps null market fields as null', () => {
    const r = PoolSchema.parse({
      chainId: 'eth',
      address: '0xabc',
      pair: '? / WETH',
      dex: 'Uniswap v2',
      model: 'v2',
      state: 'unconfirmed',
      createdAt: '2026-01-01T00:00:00Z',
      createdBlock: 1,
      createdTx: '0x1',
      token0: null,
      token1: null,
      feeTierBps: null,
      liquidityUsd: null,
      volume1hUsd: null,
      priceChange1h: null,
      buyers1h: null,
      sellers1h: null,
      risk: null,
    })
    expect(r.liquidityUsd).toBeNull()
    expect(r.price).toBeNull()
  })
})

describe('rpc summary', () => {
  it('mirrors the backend median and socket counts (subscriptions summed over every socket)', () => {
    const ep = (over: object) => ({
      id: 'x',
      chainId: 'eth',
      transport: 'ws' as const,
      url: 'wss://x',
      state: 'HEALTHY' as const,
      latencyMs: null,
      p50Ms: 40,
      p95Ms: null,
      block: 1,
      lagBlocks: 0,
      subscriptions: 2,
      uptimeSec: 10,
      reconnects24h: 0,
      ...over,
    })
    const s = summarize(
      [ep({ p50Ms: 40 }), ep({ p50Ms: 90, transport: 'http' }), ep({ p50Ms: 600, state: 'DEGRADED' }), ep({ p50Ms: 5, state: 'UNREACHABLE' })],
      'policy',
    )
    expect(s).toMatchObject({ healthy: 2, total: 4, medianP50Ms: 90, socketsTotal: 3, socketsLive: 2, subscriptions: 6 })
  })
})

describe('topic input', () => {
  it('hashes a human signature locally', () => {
    const r = parseTopicInput('PairCreated(address indexed token0, address indexed token1, address pair, uint256)')
    expect(r).toMatchObject({
      kind: 'signature',
      name: 'PairCreated',
      signature: 'PairCreated(address,address,address,uint256)',
      topic0: '0x0d3648bd0f6ba80134a33ba9275ac585d9d315f0ad8355cddefde31afa28d0e9',
    })
  })
  it('accepts a raw topic and rejects junk', () => {
    expect(parseTopicInput('0x' + 'AB'.repeat(32)).kind).toBe('topic')
    expect(parseTopicInput('0x1234').kind).toBe('invalid')
    expect(parseTopicInput('not a signature').kind).toBe('invalid')
  })
})
