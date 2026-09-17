import { computed, type MaybeRefOrGetter, toValue } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { ApiError, api, errorMessage } from '@/lib/api'
import { useStreamStore } from '@/stores/stream'
import { useToasts } from '@/stores/toasts'
import { useWallet } from '@/stores/wallet'
import type { ChainInfo, EndpointStatus, Holding, Pool, RpcHealth, RpcSummary } from '@/types/api'
import { findPool, type FeedData } from './feed'
import { qk } from './keys'

const noRetryOn = (codes: number[]) => (count: number, err: unknown) =>
  !(err instanceof ApiError && codes.includes(err.status)) && count < 2

export function useChainsQuery() {
  return useQuery({ queryKey: qk.chains, queryFn: () => api.chains(), staleTime: 60_000 })
}

export function useChainLookup() {
  const q = useChainsQuery()
  return computed(() => {
    const m = new Map<string, ChainInfo>()
    for (const c of q.data.value ?? []) m.set(c.id, c)
    return m
  })
}

/**
 * A single pool: served from the feed cache when present (so stream updates
 * show instantly), otherwise fetched.
 */
export function usePoolQuery(key: MaybeRefOrGetter<{ chainId: string; address: string } | null>) {
  const qc = useQueryClient()
  return useQuery({
    queryKey: computed(() => {
      const k = toValue(key)
      return k ? qk.pool(k.chainId, k.address) : (['pool', 'none'] as const)
    }),
    enabled: computed(() => !!toValue(key)),
    queryFn: () => {
      const k = toValue(key)!
      return api.pool(k.chainId, k.address)
    },
    initialData: () => {
      const k = toValue(key)
      if (!k) return undefined
      return findPool(qc.getQueryData<FeedData>(qk.feed), `${k.chainId}:${k.address.toLowerCase()}`) as Pool | undefined
    },
    staleTime: 30_000,
    retry: noRetryOn([404]),
  })
}

/** Mirrors the backend's summary so stream frames (endpoints only) can update it. */
export function summarize(endpoints: EndpointStatus[], failoverPolicy: string): RpcSummary {
  let healthy = 0
  let socketsTotal = 0
  let socketsLive = 0
  let subscriptions = 0
  const lat: number[] = []
  for (const e of endpoints) {
    if (e.state === 'HEALTHY') healthy++
    if (e.transport === 'ws') {
      socketsTotal++
      if (e.state !== 'UNREACHABLE') socketsLive++
      subscriptions += e.subscriptions
    }
    if (e.p50Ms !== null && e.state !== 'UNREACHABLE') lat.push(e.p50Ms)
  }
  lat.sort((a, b) => a - b)
  return {
    healthy,
    total: endpoints.length,
    medianP50Ms: lat.length ? lat[Math.floor(lat.length / 2)]! : null,
    socketsLive,
    socketsTotal,
    subscriptions,
    failoverPolicy,
  }
}

export const DEFAULT_FAILOVER_POLICY = 'auto-resubscribe on socket close, HTTP fallback after 2 drops'

export function setRpcEndpoints(prev: RpcHealth | undefined, endpoints: EndpointStatus[]): RpcHealth {
  const policy = prev?.summary.failoverPolicy ?? DEFAULT_FAILOVER_POLICY
  return { endpoints, summary: summarize(endpoints, policy) }
}

export function useRpcHealthQuery() {
  const stream = useStreamStore()
  return useQuery({
    queryKey: qk.rpcHealth,
    queryFn: () => api.rpcHealth(),
    // Stream frames keep it current while live; poll only as a fallback.
    refetchInterval: () => (stream.status === 'live' ? false : 15_000),
    staleTime: 10_000,
  })
}

export function useFundsQuery() {
  const wallet = useWallet()
  return useQuery({
    queryKey: computed(() => qk.funds(wallet.address)),
    queryFn: () => api.funds(),
    enabled: computed(() => wallet.signedIn),
    staleTime: 30_000,
    refetchInterval: 60_000,
    retry: noRetryOn([401, 501]),
  })
}

/** How often "Tokens bought" re-reads balances and prices while it is on screen. */
export const HOLDINGS_REFRESH_MS = 10_000

export function useHoldingsQuery() {
  const wallet = useWallet()
  return useQuery({
    queryKey: computed(() => qk.holdings(wallet.address)),
    queryFn: () => api.holdings(),
    enabled: computed(() => wallet.signedIn),
    // Balances, prices and P&L move while the page sits open. TanStack pauses
    // the interval while the tab is hidden, and never overlaps a slow refetch.
    refetchInterval: HOLDINGS_REFRESH_MS,
    refetchOnWindowFocus: true,
    staleTime: HOLDINGS_REFRESH_MS,
    retry: noRetryOn([401, 501]),
  })
}

/** Remove a token from "Tokens bought", dropping the row at once and restoring it if the request fails. */
export function useHideHolding() {
  const qc = useQueryClient()
  const wallet = useWallet()
  const toasts = useToasts()
  return useMutation({
    mutationFn: (h: Holding) => api.hideHolding(h.chainId, h.token.address),
    onMutate: async (h) => {
      const key = qk.holdings(wallet.address)
      await qc.cancelQueries({ queryKey: key })
      const prev = qc.getQueryData<Holding[]>(key)
      qc.setQueryData<Holding[]>(key, (old) =>
        old?.filter((x) => !(x.chainId === h.chainId && x.token.address.toLowerCase() === h.token.address.toLowerCase())),
      )
      return { key, prev }
    },
    onError: (err, h, context) => {
      if (context) qc.setQueryData(context.key, context.prev)
      toasts.push(`Could not remove ${h.token.symbol}: ${errorMessage(err)}`, 'danger')
    },
    // A periodic refetch that raced the request may have put the row back.
    onSettled: (_data, _err, _h, context) => {
      if (context) void qc.invalidateQueries({ queryKey: context.key })
    },
  })
}

export function usePositionsQuery() {
  const wallet = useWallet()
  return useQuery({
    queryKey: computed(() => qk.positions(wallet.address)),
    queryFn: () => api.positions(),
    enabled: computed(() => wallet.signedIn),
    staleTime: 30_000,
    retry: noRetryOn([401, 501]),
  })
}

export function useTokenWatchesQuery() {
  const wallet = useWallet()
  return useQuery({
    queryKey: computed(() => qk.tokenWatches(wallet.address)),
    queryFn: () => api.tokenWatches(),
    enabled: computed(() => wallet.signedIn),
    retry: noRetryOn([401]),
  })
}

export function useTopicWatchesQuery() {
  const wallet = useWallet()
  return useQuery({
    queryKey: computed(() => qk.topicWatches(wallet.address)),
    queryFn: () => api.topicWatches(),
    enabled: computed(() => wallet.signedIn),
    retry: noRetryOn([401]),
  })
}
