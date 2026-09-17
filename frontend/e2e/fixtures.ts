/**
 * Design fixtures for end-to-end tests only. They mirror the prototype's
 * sample data in the backend's wire shapes and must never be imported by
 * application code.
 */

const minutesAgo = (m: number) => new Date(Date.now() - m * 60_000).toISOString()
const hex = (c: string, n: number) => '0x' + c.repeat(n)

export const WALLET = '0x7a4f00000000000000000000000000000000' + '2be1'

export const CHAINS = [
  { id: 'eth', name: 'Ethereum', tag: 'ETH', kind: 'evm', numericId: 1, explorer: 'https://etherscan.io', nativeSymbol: 'ETH', head: 20846113, ingesting: true, dexes: ['Uniswap v2', 'Uniswap v3', 'Uniswap v4', 'Curve'] },
  { id: 'base', name: 'Base', tag: 'BASE', kind: 'evm', numericId: 8453, explorer: 'https://basescan.org', nativeSymbol: 'ETH', head: 18402771, ingesting: true, wrappedNative: '0x4200000000000000000000000000000000000006', stables: [], dexes: ['Uniswap v2', 'Aerodrome'] },
  { id: 'bsc', name: 'BSC', tag: 'BSC', kind: 'evm', numericId: 56, explorer: 'https://bscscan.com', nativeSymbol: 'BNB', head: 41229004, ingesting: true },
  { id: 'arb', name: 'Arbitrum', tag: 'ARB', kind: 'evm', numericId: 42161, explorer: 'https://arbiscan.io', nativeSymbol: 'ETH', head: 249118840, ingesting: true },
  { id: 'sol', name: 'Solana', tag: 'SOL', kind: 'solana', explorer: 'https://solscan.io', nativeSymbol: 'SOL', head: 0, ingesting: false },
]

const check = (label: string, value: string, ok: boolean) => ({ label, value, ok, known: true })

function risk(score: number, o: { sellers: number; ratio: number; lp: string; mint: boolean; tax: string; top: number; holders: number }) {
  return {
    score,
    band: score < 25 ? 'SAFE' : score < 60 ? 'WATCH' : 'DANGER',
    verdict: '',
    complete: true,
    scoredAt: minutesAgo(0),
    checks: [
      check('Unique sellers', `${o.sellers.toLocaleString('en-US')} addresses`, o.sellers > 20),
      check('Buy / sell address ratio', `${o.ratio.toFixed(1)} : 1`, o.ratio < 8),
      check('LP lock', o.lp, o.lp.startsWith('Locked')),
      check('Mint authority', o.mint ? 'Active — owner can mint' : 'Revoked', !o.mint),
      check('Buy / sell tax', `${o.tax} %`, o.tax === '0 / 0'),
      check('Top 10 holders', `${o.top.toFixed(1)} % of supply`, o.top < 25),
      check('Holders', String(o.holders), o.holders > 100),
      check('Contract verified', score > 60 ? 'Unverified source' : 'Verified', score <= 60),
    ],
  }
}

function pool(p: {
  chainId: string
  addr: string
  pair: string
  dex: string
  model: 'v2' | 'clmm' | 'stable'
  age: number
  liq: number
  vol: number
  change: number
  buyers: number
  sellers: number
  score: number
  lp: string
  mint: boolean
  tax: string
  top: number
  holders: number
  price?: number
  state?: string
}) {
  const [s0, s1] = p.pair.split(' / ')
  return {
    chainId: p.chainId,
    address: p.addr,
    pair: p.pair,
    dex: p.dex,
    model: p.model,
    state: p.state ?? 'confirmed',
    createdAt: minutesAgo(p.age),
    createdBlock: 100,
    createdTx: hex('1', 64),
    token0: { address: hex('b', 40), symbol: s0, decimals: 18 },
    // Base's WETH is the real one, so pairs priced in it can be paid in ETH.
    token1: { address: p.chainId === 'base' && s1 === 'WETH' ? '0x4200000000000000000000000000000000000006' : hex('c', 40), symbol: s1, decimals: s1 === 'USDC' || s1 === 'USDT' ? 6 : 18 },
    feeTierBps: p.model === 'clmm' ? 30 : 25,
    liquidityUsd: p.liq,
    volume1hUsd: p.vol,
    priceChange1h: p.change,
    buyers1h: p.buyers,
    sellers1h: p.sellers,
    price: p.price ?? null,
    risk: risk(p.score, { sellers: p.sellers, ratio: p.buyers / Math.max(1, p.sellers), lp: p.lp, mint: p.mint, tax: p.tax, top: p.top, holders: p.holders }),
  }
}

export const POOL_GROK = hex('1', 40)
export const POOL_AERO = hex('2', 40)
export const POOL_PEPE = hex('3', 40)

export const POOLS = [
  pool({ chainId: 'eth', addr: POOL_GROK, pair: 'GROK / WETH', dex: 'Uniswap v3', model: 'clmm', age: 2, liq: 412000, vol: 188000, change: 64.2, buyers: 412, sellers: 188, score: 12, lp: 'Locked 12mo', mint: false, tax: '0 / 0', top: 8.4, holders: 218, price: 0.00418, state: 'unconfirmed' }),
  pool({ chainId: 'base', addr: POOL_AERO, pair: 'AERO / USDC', dex: 'Aerodrome', model: 'v2', age: 7, liq: 1240000, vol: 512000, change: 8.4, buyers: 1840, sellers: 1207, score: 6, lp: 'Locked 24mo', mint: false, tax: '0 / 0', top: 4.1, holders: 1902 }),
  pool({ chainId: 'bsc', addr: POOL_PEPE, pair: 'PEPE2 / WBNB', dex: 'PancakeSwap v2', model: 'v2', age: 16, liq: 41200, vol: 302000, change: 488.0, buyers: 1214, sellers: 3, score: 91, lp: 'Unlocked', mint: true, tax: '12 / 24', top: 68.9, holders: 34 }),
  pool({ chainId: 'arb', addr: hex('4', 40), pair: 'ARBI / WETH', dex: 'Camelot v3', model: 'clmm', age: 22, liq: 384000, vol: 91200, change: -4.8, buyers: 342, sellers: 289, score: 22, lp: 'Locked 9mo', mint: false, tax: '0 / 0', top: 14.6, holders: 388, price: 0.2185 }),
  pool({ chainId: 'base', addr: hex('5', 40), pair: 'DEGEN9 / WETH', dex: 'Uniswap v2', model: 'v2', age: 34, liq: 28800, vol: 74100, change: -38.2, buyers: 286, sellers: 9, score: 83, lp: 'Unlocked', mint: true, tax: '8 / 8', top: 55.4, holders: 22 }),
  pool({ chainId: 'eth', addr: hex('6', 40), pair: 'STETH / WETH', dex: 'Curve', model: 'stable', age: 41, liq: 4820000, vol: 1120000, change: 0.2, buyers: 3980, sellers: 3411, score: 4, lp: 'Locked 36mo', mint: false, tax: '0 / 0', top: 3.2, holders: 5104 }),
  pool({ chainId: 'bsc', addr: hex('7', 40), pair: 'TURBO / USDT', dex: 'PancakeSwap v2', model: 'v2', age: 48, liq: 132000, vol: 58400, change: 16.9, buyers: 221, sellers: 158, score: 44, lp: 'Locked 1mo', mint: false, tax: '3 / 3', top: 28.1, holders: 208 }),
]

export function rpcHealth() {
  const ep = (id: string, chainId: string, transport: 'ws' | 'http', url: string, state: string, p50: number | null, over: object = {}) => ({
    id,
    chainId,
    transport,
    url,
    state,
    latencyMs: p50,
    p50Ms: p50,
    p95Ms: p50 === null ? null : p50 * 2,
    block: state === 'UNREACHABLE' ? null : 20846113,
    lagBlocks: 0,
    subscriptions: transport === 'ws' && state !== 'UNREACHABLE' ? 3 : 0,
    uptimeSec: transport === 'ws' ? 22320 : null,
    reconnects24h: 0,
    pollIntervalMs: transport === 'http' ? 400 : undefined,
    successRate: 0.99,
    ...over,
  })
  const endpoints = [
    ep('eth-ws-publicnode', 'eth', 'ws', 'wss://ethereum-rpc.publicnode.com', 'HEALTHY', 38),
    ep('eth-http-llamarpc', 'eth', 'http', 'https://eth.llamarpc.com', 'HEALTHY', 92),
    ep('base-ws-publicnode', 'base', 'ws', 'wss://base-rpc.publicnode.com', 'HEALTHY', 46, { reconnects24h: 1 }),
    ep('bsc-ws-publicnode', 'bsc', 'ws', 'wss://bsc-rpc.publicnode.com', 'DEGRADED', 612, { lagBlocks: 3, reconnects24h: 6 }),
    ep('arb-http-official', 'arb', 'http', 'https://arb1.arbitrum.io/rpc', 'UNREACHABLE', null, { reconnects24h: 12, lastError: 'dial tcp: i/o timeout' }),
  ]
  return {
    endpoints,
    summary: { healthy: 3, total: 5, medianP50Ms: 46, socketsLive: 3, socketsTotal: 3, subscriptions: 9, failoverPolicy: 'auto-resubscribe on socket close, HTTP fallback after 2 drops' },
  }
}

export const FUNDS = {
  chains: [
    { chainId: 'eth', name: 'Ethereum', tag: 'ETH', totalUsd: 7270, assets: [
      { symbol: 'ETH', amount: '1.24', decimals: 18, usd: 4120, isStable: false, isNative: true },
      { symbol: 'USDC', amount: '3150.00', decimals: 6, usd: 3150, isStable: true, isNative: false },
      { symbol: 'DAI', amount: '0', decimals: 18, usd: 0, isStable: true, isNative: false },
    ] },
    { chainId: 'base', name: 'Base', tag: 'BASE', totalUsd: 8260, assets: [
      { symbol: 'ETH', amount: '0.86', decimals: 18, usd: 2860, isStable: false, isNative: true },
      { symbol: 'USDC', amount: '5400.00', decimals: 6, usd: 5400, isStable: true, isNative: false },
    ] },
    { chainId: 'bsc', name: 'BSC', tag: 'BSC', totalUsd: 6690, assets: [
      { symbol: 'BNB', amount: '6.4', decimals: 18, usd: 3940, isStable: false, isNative: true },
      { symbol: 'USDT', amount: '2750.00', decimals: 18, usd: 2750, isStable: true, isNative: false },
    ] },
    { chainId: 'arb', name: 'Arbitrum', tag: 'ARB', totalUsd: null, assets: [
      { symbol: 'ETH', amount: '0.42', decimals: 18, usd: null, isStable: false, isNative: true },
    ] },
    { chainId: 'rbn', name: 'Robinhood', tag: 'RBN', totalUsd: 0, assets: [
      { symbol: 'ETH', amount: '0', decimals: 18, usd: 0, isStable: false, isNative: true },
      { symbol: 'USDG', amount: '0', decimals: 6, usd: 0, isStable: true, isNative: false },
    ] },
  ],
  stables: 11300,
  totalUsd: 22220,
}
