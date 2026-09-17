/**
 * Filtering the feed by DEX — "only Uniswap v3", say.
 *
 * The options come from the backend's chain list, which reports the DEXes each
 * chain's ingester actually watches. A hardcoded list here would go stale the
 * moment a factory is added to the backend's config, and would offer filters
 * that can never match anything.
 *
 * The filter lives in the URL like chain and risk, as a slug: `dex=uniswap-v3`.
 * Slugs travel instead of raw names because a name carries spaces and case that
 * would need escaping and would break the moment either changed cosmetically.
 */
import type { ChainInfo, Pool } from '@/types/api'

/** A DEX name as a URL-safe slug: "Uniswap v3" → "uniswap-v3". */
export function dexSlug(name: string): string {
  return name.trim().toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '')
}

export const ALL_DEXES = 'all'

/**
 * The DEXes offered, in config order, deduped across chains. Narrowed to one
 * chain when one is selected: a Uniswap v2 chip on a chain with no v2 factory
 * would only ever produce an empty feed.
 */
export function dexOptions(chains: ChainInfo[] | undefined, chainId: string): string[] {
  const out: string[] = []
  for (const c of chains ?? []) {
    if (chainId !== ALL_DEXES && c.id !== chainId) continue
    for (const d of c.dexes ?? []) if (!out.includes(d)) out.push(d)
  }
  return out
}

/** Whether a pool belongs to the selected DEX. `all` matches everything. */
export function matchesDex(p: Pool, selected: string): boolean {
  return selected === ALL_DEXES || dexSlug(p.dex) === selected
}

/**
 * The selected slug read back from the URL, kept only when some listed DEX
 * still matches it — a slug left over from another chain would silently empty
 * the feed.
 */
export function dexFromQuery(value: unknown, options: string[]): string {
  if (typeof value !== 'string' || !value || value === ALL_DEXES) return ALL_DEXES
  return options.some((d) => dexSlug(d) === value) ? value : ALL_DEXES
}
