/**
 * Static chain presentation: colour, ordering, explorer and DexScreener slugs,
 * expected block time. This is metadata, not market data — which chains are
 * actually configured and ingesting always comes from `GET /chains`.
 */
import type { ChainInfo, Pool, Token } from '@/types/api'

/** How Uniswap v4 names a chain's native currency as a pool token. */
export const NATIVE_ADDRESS = '0x0000000000000000000000000000000000000000'

/**
 * Which side of a pool is the token being traded and which is its quote,
 * decided the way the backend decides it: the quote is a configured stable,
 * or failing that the native currency or its wrapper. Slot order says nothing
 * — token0 is just the lower address, so native ETH sits in slot 0 of most v4
 * pairs, and reading slot 0 as "the token" builds a buy of ETH.
 *
 * Without the chain's quote assets (not loaded yet), the backend-built pair
 * name, "TOKEN / QUOTE", tells the sides apart.
 */
export function orientPool(pool: Pick<Pool, 'pair' | 'token0' | 'token1'>, info?: ChainInfo | null): { base: Token | null; quote: Token | null } {
  const t0 = pool.token0
  const t1 = pool.token1
  if (info && (info.stables.length > 0 || info.wrappedNative)) {
    const wrapped = info.wrappedNative?.toLowerCase()
    const rank = (t: Token | null) => {
      if (!t) return 0
      const a = t.address.toLowerCase()
      if (info.stables.includes(a)) return 2
      return a === NATIVE_ADDRESS || a === wrapped ? 1 : 0
    }
    return rank(t0) > rank(t1) ? { base: t1, quote: t0 } : { base: t0, quote: t1 }
  }
  const baseSymbol = pool.pair.split(' / ')[0]
  if (t0 && t1 && t1.symbol === baseSymbol && t0.symbol !== baseSymbol) return { base: t1, quote: t0 }
  return { base: t0, quote: t1 }
}

export interface ChainMeta {
  id: string
  name: string
  tag: string
  dot: string
  /** Expected block time in seconds; drives the BlockPulse amber threshold. */
  blockTime: number
  numericId?: number
  explorer?: string
  dexscreener?: string
}

export const CHAIN_META: ChainMeta[] = [
  { id: 'eth', name: 'Ethereum', tag: 'ETH', dot: 'var(--chain-eth)', blockTime: 12, numericId: 1, explorer: 'https://etherscan.io', dexscreener: 'ethereum' },
  { id: 'base', name: 'Base', tag: 'BASE', dot: 'var(--chain-base)', blockTime: 2, numericId: 8453, explorer: 'https://basescan.org', dexscreener: 'base' },
  { id: 'bsc', name: 'BSC', tag: 'BSC', dot: 'var(--chain-bsc)', blockTime: 3, numericId: 56, explorer: 'https://bscscan.com', dexscreener: 'bsc' },
  { id: 'arb', name: 'Arbitrum', tag: 'ARB', dot: 'var(--chain-arb)', blockTime: 1, numericId: 42161, explorer: 'https://arbiscan.io', dexscreener: 'arbitrum' },
  { id: 'rbn', name: 'Robinhood', tag: 'RBN', dot: 'var(--chain-rbn)', blockTime: 1, numericId: 4663, explorer: 'https://robinhoodchain.blockscout.com', dexscreener: 'robinhood' },
  { id: 'arc', name: 'Arc', tag: 'ARC', dot: 'var(--chain-arc)', blockTime: 0.5, numericId: 5042, explorer: 'https://explorer.arc.io', dexscreener: 'arc' },
  { id: 'sol', name: 'Solana', tag: 'SOL', dot: 'var(--chain-sol)', blockTime: 0.4, explorer: 'https://solscan.io', dexscreener: 'solana' },
  { id: 'sui', name: 'SUI', tag: 'SUI', dot: 'var(--chain-sui)', blockTime: 1, explorer: 'https://suivision.xyz', dexscreener: 'sui' },
]

const byId = new Map(CHAIN_META.map((c) => [c.id, c]))

export function chainMeta(id: string, info?: ChainInfo | null): ChainMeta {
  const known = byId.get(id)
  return {
    id,
    name: info?.name ?? known?.name ?? id.toUpperCase(),
    tag: info?.tag ?? known?.tag ?? id.slice(0, 4).toUpperCase(),
    dot: known?.dot ?? 'var(--chain-unknown)',
    blockTime: known?.blockTime ?? 12,
    numericId: info?.numericId ?? known?.numericId,
    explorer: info?.explorer ?? known?.explorer,
    dexscreener: known?.dexscreener,
  }
}

export function explorerTx(chainId: string, tx: string, info?: ChainInfo | null): string | null {
  const base = chainMeta(chainId, info).explorer
  return base && tx ? `${base.replace(/\/$/, '')}/tx/${tx}` : null
}

export function explorerAddress(chainId: string, address: string, info?: ChainInfo | null): string | null {
  const base = chainMeta(chainId, info).explorer
  return base && address ? `${base.replace(/\/$/, '')}/address/${address}` : null
}

export function dexscreenerUrl(chainId: string, poolAddress: string, pair: string): string {
  const slug = chainMeta(chainId).dexscreener
  if (slug) return `https://dexscreener.com/${slug}/${poolAddress}`
  return `https://dexscreener.com/search?q=${encodeURIComponent(pair.replace(' / ', '/'))}`
}

export function poolKey(chainId: string, address: string): string {
  return `${chainId}:${address.toLowerCase()}`
}

export function parsePoolKey(key: string | null | undefined): { chainId: string; address: string } | null {
  if (!key) return null
  const i = key.indexOf(':')
  if (i <= 0 || i === key.length - 1) return null
  return { chainId: key.slice(0, i), address: key.slice(i + 1) }
}
