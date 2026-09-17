/**
 * The pool feed lives in one TanStack infinite query. Stream frames and the
 * polling fallback both write into that cache through these helpers, so there
 * is exactly one copy of each pool.
 */
import type { InfiniteData, QueryClient } from '@tanstack/vue-query'
import { useInfiniteQuery } from '@tanstack/vue-query'
import { ref } from 'vue'
import { api } from '@/lib/api'
import { poolKey } from '@/lib/chains'
import type { Pool, PoolStateUpdate } from '@/types/api'
import { qk } from './keys'

export interface FeedPage {
  data: Pool[]
  nextCursor: string | null
}
export type FeedData = InfiniteData<FeedPage, string | null>

export const FEED_PAGE_SIZE = 200

export function useFeedQuery() {
  return useInfiniteQuery<FeedPage, Error, FeedData, typeof qk.feed, string | null>({
    queryKey: qk.feed,
    queryFn: ({ pageParam, signal }) => api.pools({ sort: 'age', limit: FEED_PAGE_SIZE, cursor: pageParam }, signal),
    initialPageParam: null,
    getNextPageParam: (last) => last.nextCursor,
    // The stream keeps this fresh; a refetch would reload every page.
    staleTime: 5 * 60_000,
    refetchOnWindowFocus: false,
  })
}

const keyOf = (p: Pick<Pool, 'chainId' | 'address'>) => poolKey(p.chainId, p.address)

/** Rows that arrived live, for the insert highlight. Key -> arrival time. */
export const arrivals = ref(new Map<string, number>())
/** Monotonic count of live inserts; the table watches it to hold scroll position. */
export const insertCounter = ref(0)
/** Rows fading out after a reorg. */
export const leaving = ref(new Set<string>())

/** Insert new pools at the top, newest first; existing ones are replaced in place. */
export function insertPools(data: FeedData | undefined, pools: Pool[]): { data: FeedData | undefined; inserted: string[] } {
  if (!data || data.pages.length === 0) return { data, inserted: [] }
  const index = new Map<string, true>()
  for (const page of data.pages) for (const p of page.data) index.set(keyOf(p), true)

  const replacements = new Map<string, Pool>()
  const fresh: Pool[] = []
  for (const p of pools) {
    if (p.state === 'reorged') continue
    const k = keyOf(p)
    if (index.has(k)) replacements.set(k, p)
    else if (!fresh.some((f) => keyOf(f) === k)) fresh.push(p)
  }
  if (fresh.length === 0 && replacements.size === 0) return { data, inserted: [] }

  fresh.sort((a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt))
  const pages = data.pages.map((page, i) => {
    let rows = replacements.size ? page.data.map((p) => replacements.get(keyOf(p)) ?? p) : page.data
    if (i === 0 && fresh.length) rows = [...fresh, ...rows]
    return { ...page, data: rows }
  })
  return { data: { ...data, pages }, inserted: fresh.map(keyOf) }
}

/** Merge a full pool or a state-only update into an existing row. */
export function patchPool(data: FeedData | undefined, update: Pool | PoolStateUpdate): FeedData | undefined {
  if (!data) return data
  const k = keyOf(update)
  let hit = false
  const pages = data.pages.map((page) => {
    const idx = page.data.findIndex((p) => keyOf(p) === k)
    if (idx === -1) return page
    hit = true
    const rows = page.data.slice()
    rows[idx] = { ...rows[idx]!, ...update } as Pool
    return { ...page, data: rows }
  })
  return hit ? { ...data, pages } : data
}

export function removePool(data: FeedData | undefined, chainId: string, address: string): FeedData | undefined {
  if (!data) return data
  const k = poolKey(chainId, address)
  return {
    ...data,
    pages: data.pages.map((page) => ({ ...page, data: page.data.filter((p) => keyOf(p) !== k) })),
  }
}

export function findPool(data: FeedData | undefined, key: string): Pool | undefined {
  if (!data) return undefined
  for (const page of data.pages) {
    const p = page.data.find((x) => keyOf(x) === key)
    if (p) return p
  }
  return undefined
}

export function flattenFeed(data: FeedData | undefined): Pool[] {
  if (!data) return []
  const seen = new Set<string>()
  const out: Pool[] = []
  for (const page of data.pages) {
    for (const p of page.data) {
      const k = keyOf(p)
      if (seen.has(k)) continue
      seen.add(k)
      out.push(p)
    }
  }
  return out
}

/** Apply the effects of pools arriving live: cache insert plus highlight bookkeeping. */
export function applyNewPools(qc: QueryClient, pools: Pool[]) {
  let inserted: string[] = []
  qc.setQueryData<FeedData>(qk.feed, (old) => {
    const r = insertPools(old, pools)
    inserted = r.inserted
    return r.data
  })
  for (const p of pools) qc.setQueryData(qk.pool(p.chainId, p.address), p)
  if (inserted.length) {
    const now = Date.now()
    const next = new Map(arrivals.value)
    for (const k of inserted) next.set(k, now)
    // Keep the map from growing with the feed.
    for (const [k, t] of next) if (now - t > 5000) next.delete(k)
    arrivals.value = next
    insertCounter.value += inserted.length
  }
  return inserted
}
