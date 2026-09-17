/**
 * Favorite pairs: starred rows that move out of the feed into their own
 * section, and that the backend keeps watching for as long as they stay
 * starred. The favorites query is the one source of truth for whether a pair
 * is starred — a row's own `favoritedAt` can lag a star made a moment ago.
 */
import { useMutation, useQuery, useQueryClient, type QueryClient } from '@tanstack/vue-query'
import { api, errorMessage } from '@/lib/api'
import { poolKey } from '@/lib/chains'
import { useStreamStore } from '@/stores/stream'
import { useToasts } from '@/stores/toasts'
import type { Pool } from '@/types/api'
import { findPool, type FeedData } from './feed'
import { qk } from './keys'

const keyOf = (p: Pick<Pool, 'chainId' | 'address'>) => poolKey(p.chainId, p.address)

export function useFavoritesQuery() {
  const stream = useStreamStore()
  return useQuery({
    queryKey: qk.favorites,
    queryFn: ({ signal }) => api.favorites(signal),
    staleTime: 30_000,
    // Stream frames keep the figures current while live; poll only as a fallback.
    refetchInterval: () => (stream.status === 'live' ? false : 15_000),
  })
}

export function favoriteKeys(list: Pool[] | undefined): Set<string> {
  return new Set((list ?? []).map(keyOf))
}

/** Add a starred pool at the top, or replace it in place if already listed. */
export function addFavorite(list: Pool[] | undefined, pool: Pool): Pool[] {
  const k = keyOf(pool)
  const current = list ?? []
  return current.some((p) => keyOf(p) === k) ? current.map((p) => (keyOf(p) === k ? pool : p)) : [pool, ...current]
}

export function dropFavorite(list: Pool[] | undefined, chainId: string, address: string): Pool[] | undefined {
  if (!list) return list
  const k = poolKey(chainId, address)
  return list.filter((p) => keyOf(p) !== k)
}

/** Merge a pool update into a listed favorite; unlisted pools are ignored. */
export function patchFavorite(list: Pool[] | undefined, update: Partial<Pool> & Pick<Pool, 'chainId' | 'address'>): Pool[] | undefined {
  if (!list) return list
  const k = keyOf(update)
  let hit = false
  const next = list.map((p) => {
    if (keyOf(p) !== k) return p
    hit = true
    return { ...p, ...update }
  })
  return hit ? next : list
}

/**
 * Apply a star or unstar — made here or in another tab — to the favorites
 * cache. A star for a pool this tab has never loaded refetches the list.
 */
export function applyFavoriteChange(qc: QueryClient, chainId: string, address: string, favoritedAt: string | null, pool?: Pool) {
  if (favoritedAt === null) {
    qc.setQueryData<Pool[]>(qk.favorites, (old) => dropFavorite(old, chainId, address))
    return
  }
  const known =
    pool ?? qc.getQueryData<Pool>(qk.pool(chainId, address)) ?? findPool(qc.getQueryData<FeedData>(qk.feed), poolKey(chainId, address))
  if (!known || qc.getQueryData<Pool[]>(qk.favorites) === undefined) {
    void qc.invalidateQueries({ queryKey: qk.favorites })
    return
  }
  qc.setQueryData<Pool[]>(qk.favorites, (old) => addFavorite(old, { ...known, favoritedAt }))
}

export function useToggleFavorite() {
  const qc = useQueryClient()
  const toasts = useToasts()
  return useMutation({
    mutationFn: ({ pool, star }: { pool: Pool; star: boolean }) =>
      star ? api.addFavorite(pool.chainId, pool.address) : api.removeFavorite(pool.chainId, pool.address).then(() => null),
    onMutate: async ({ pool, star }) => {
      await qc.cancelQueries({ queryKey: qk.favorites })
      const prev = qc.getQueryData<Pool[]>(qk.favorites)
      const at = star ? new Date().toISOString() : null
      applyFavoriteChange(qc, pool.chainId, pool.address, at, star ? { ...pool, favoritedAt: at } : undefined)
      return { prev }
    },
    onSuccess: (saved, { pool }) => {
      // The server's copy carries the real star time and its latest figures.
      if (saved) applyFavoriteChange(qc, pool.chainId, pool.address, saved.favoritedAt ?? new Date().toISOString(), saved)
    },
    onError: (err, { pool, star }, context) => {
      qc.setQueryData(qk.favorites, context?.prev)
      toasts.push(`Could not ${star ? 'star' : 'unstar'} ${pool.pair}: ${errorMessage(err)}`, 'danger')
    },
  })
}
