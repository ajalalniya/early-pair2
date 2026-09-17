import type { Pool } from '@/types/api'

/** Test-only pool builder. Never imported by application code. */
export function makePool(over: Partial<Pool> = {}): Pool {
  return {
    chainId: 'base',
    address: '0x' + 'a'.repeat(40),
    pair: 'AERO / USDC',
    dex: 'Aerodrome',
    model: 'v2',
    state: 'confirmed',
    createdAt: '2026-01-01T12:00:00Z',
    createdBlock: 1,
    createdTx: '0x' + '1'.repeat(64),
    token0: { address: '0x' + 'b'.repeat(40), symbol: 'AERO', decimals: 18 },
    token1: { address: '0x' + 'c'.repeat(40), symbol: 'USDC', decimals: 6 },
    feeTierBps: 30,
    liquidityUsd: 1_240_000,
    volume1hUsd: 512_000,
    priceChange1h: 8.4,
    buyers1h: 1840,
    sellers1h: 1207,
    tradersPending: null,
    favoritedAt: null,
    removedAt: null,
    importedAt: null,
    price: null,
    risk: { score: 6, band: 'SAFE', verdict: '', checks: [], complete: true },
    ...over,
  }
}
