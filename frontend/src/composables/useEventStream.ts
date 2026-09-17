/**
 * The live stream. One socket for the whole app, written straight into the
 * TanStack cache.
 *
 * - Reconnects on close and error with exponential backoff and jitter
 *   (0.5 s, doubling, capped at 15 s).
 * - Resubscribes with the last seen `seq`, so the server replays what we
 *   missed; if its buffer cannot reach back that far it says `resync` and we
 *   refetch instead of pretending continuity.
 * - Treats 45 s without any inbound frame as a dead socket. Browsers do not
 *   surface protocol pings, so we also send an application ping every 20 s to
 *   guarantee traffic on a quiet feed.
 * - While disconnected, polls `GET /pools` every 5 s.
 * - Frames are validated with zod and applied in one batch per animation
 *   frame; a malformed frame is dropped, never thrown.
 */
import { useQueryClient, type QueryClient } from '@tanstack/vue-query'
import { api } from '@/lib/api'
import { streamUrl } from '@/lib/env'
import { poolKey } from '@/lib/chains'
import { applyFavoriteChange, dropFavorite, patchFavorite } from '@/queries/favorites'
import { applyNewPools, leaving, patchPool, removePool, type FeedData } from '@/queries/feed'
import { qk } from '@/queries/keys'
import { setRpcEndpoints } from '@/queries'
import { useStreamStore } from '@/stores/stream'
import { useToasts } from '@/stores/toasts'
import {
  EndpointStatusSchema,
  FrameSchema,
  HeadDataSchema,
  PoolSchema,
  PoolStateUpdateSchema,
  SubscribedAckSchema,
  WatchHitDataSchema,
  type EndpointStatus,
  type Frame,
  type Pool,
  type PoolStateUpdate,
  type RpcHealth,
  type TokenWatch,
  type TopicWatch,
  type WatchHitData,
} from '@/types/api'

export const STREAM_TOPICS = ['pools.new', 'pools.update', 'rpc.health', 'watch.hits', 'head'] as const

export const BACKOFF_MIN_MS = 500
export const BACKOFF_MAX_MS = 15_000
export const PING_EVERY_MS = 20_000
export const SILENCE_LIMIT_MS = 45_000
export const POLL_EVERY_MS = 5_000

/** Full-jitter-ish backoff: a random delay in [base/2, base]. */
export function backoffDelay(attempt: number, random: () => number = Math.random): number {
  const base = Math.min(BACKOFF_MAX_MS, BACKOFF_MIN_MS * 2 ** attempt)
  return Math.round(base / 2 + random() * (base / 2))
}

type Store = ReturnType<typeof useStreamStore>
type Toasts = ReturnType<typeof useToasts>

interface Handle {
  stop(): void
  reconnectNow(): void
}

let singleton: Handle | null = null

export function useEventStream(): Handle {
  if (singleton) return singleton
  singleton = createStream(useQueryClient(), useStreamStore(), useToasts())
  return singleton
}

function createStream(qc: QueryClient, store: Store, toasts: Toasts): Handle {
  let ws: WebSocket | null = null
  let attempt = 0
  let stopped = false
  let lastInbound = 0
  let reconnectTimer: number | undefined
  let pingTimer: number | undefined
  let watchdogTimer: number | undefined
  let pollTimer: number | undefined
  let flushHandle: number | undefined
  let queue: Frame[] = []

  // ------------------------------------------------------------ socket

  function open() {
    if (stopped) return
    store.connecting = true
    let sock: WebSocket
    try {
      sock = new WebSocket(streamUrl())
    } catch {
      onDown()
      return
    }
    ws = sock
    lastInbound = Date.now()

    sock.onopen = () => {
      if (ws !== sock) return
      const frame: Record<string, unknown> = { op: 'sub', topics: STREAM_TOPICS, filters: { chains: [] } }
      if (store.lastSeq !== null) frame.since = store.lastSeq
      sock.send(JSON.stringify(frame))
      startHeartbeat(sock)
    }
    sock.onmessage = (ev) => {
      if (ws !== sock) return
      lastInbound = Date.now()
      handleMessage(ev.data)
    }
    sock.onerror = () => {
      if (ws === sock) sock.close()
    }
    sock.onclose = () => {
      if (ws === sock) onDown()
    }
  }

  function detach() {
    window.clearInterval(pingTimer)
    window.clearInterval(watchdogTimer)
    if (ws) {
      ws.onopen = ws.onmessage = ws.onerror = ws.onclose = null
      try {
        ws.close()
      } catch {
        /* already closed */
      }
      ws = null
    }
  }

  function onDown() {
    detach()
    store.connecting = false
    if (stopped) return
    if (store.status === 'live') store.status = 'polling'
    startPolling()
    window.clearTimeout(reconnectTimer)
    reconnectTimer = window.setTimeout(open, backoffDelay(attempt++))
  }

  function startHeartbeat(sock: WebSocket) {
    window.clearInterval(pingTimer)
    window.clearInterval(watchdogTimer)
    pingTimer = window.setInterval(() => {
      if (sock.readyState === WebSocket.OPEN) sock.send('{"op":"ping"}')
    }, PING_EVERY_MS)
    // An open-but-silent socket looks healthy forever unless we check.
    watchdogTimer = window.setInterval(() => {
      if (Date.now() - lastInbound > SILENCE_LIMIT_MS) onDown()
    }, 5_000)
  }

  // ------------------------------------------------------------ polling

  function startPolling() {
    if (pollTimer !== undefined) return
    const tick = async () => {
      try {
        const page = await api.pools({ sort: 'age', limit: 100 })
        if (store.status !== 'live') store.status = 'polling'
        mergePolled(page.data)
      } catch {
        if (store.status !== 'live') store.status = 'down'
      }
      void qc.invalidateQueries({ queryKey: qk.rpcHealth })
    }
    void tick()
    pollTimer = window.setInterval(tick, POLL_EVERY_MS)
  }

  function stopPolling() {
    window.clearInterval(pollTimer)
    pollTimer = undefined
  }

  function mergePolled(pools: Pool[]) {
    const current = qc.getQueryData<FeedData>(qk.feed)
    if (!current) {
      void qc.invalidateQueries({ queryKey: qk.feed })
      return
    }
    // Polled rows replace in place and new ones go to the top, same as live.
    applyNewPools(qc, pools)
  }

  // ------------------------------------------------------------ frames

  function handleMessage(raw: unknown) {
    if (typeof raw !== 'string') return
    let json: unknown
    try {
      json = JSON.parse(raw)
    } catch {
      console.warn('[stream] dropped a non-JSON frame')
      return
    }
    const env = FrameSchema.safeParse(json)
    if (!env.success) {
      console.warn('[stream] dropped a malformed frame', env.error.issues)
      return
    }
    const f = env.data

    if (f.t === 'pong') return
    if (f.t === 'subscribed') {
      const ack = SubscribedAckSchema.safeParse(json)
      attempt = 0
      store.connecting = false
      store.status = 'live'
      stopPolling()
      if (!ack.success) return
      if (ack.data.resync) {
        store.lastSeq = ack.data.seq
        resync()
      } else if (store.lastSeq === null) {
        store.lastSeq = ack.data.seq
      }
      return
    }
    if (f.t === 'resync') {
      // The server dropped us for lag; it closes next. Start clean.
      store.lastSeq = null
      resync()
      return
    }

    if (typeof f.seq === 'number') store.lastSeq = Math.max(store.lastSeq ?? 0, f.seq)
    queue.push(f)
    scheduleFlush()
  }

  function resync() {
    void qc.invalidateQueries({ queryKey: qk.feed })
    void qc.invalidateQueries({ queryKey: qk.favorites })
    void qc.invalidateQueries({ queryKey: qk.rpcHealth })
    void qc.invalidateQueries({ queryKey: ['watch'] })
  }

  function scheduleFlush() {
    if (flushHandle !== undefined) return
    const run = () => {
      flushHandle = undefined
      const batch = queue
      queue = []
      applyBatch(batch)
    }
    // rAF does not fire in a background tab; do not let the queue pile up there.
    flushHandle = document.hidden ? window.setTimeout(run, 250) : window.requestAnimationFrame(run)
  }

  function applyBatch(frames: Frame[]) {
    const newPools: Pool[] = []
    const updates: (Pool | PoolStateUpdate)[] = []
    let endpoints: EndpointStatus[] | null = null
    const hits: WatchHitData[] = []

    for (const f of frames) {
      switch (f.t) {
        case 'pools.new': {
          const r = PoolSchema.safeParse(f.d)
          if (r.success) newPools.push(r.data)
          else console.warn('[stream] dropped a malformed pools.new', r.error.issues)
          break
        }
        case 'pools.update': {
          const full = PoolSchema.safeParse(f.d)
          if (full.success) {
            updates.push(full.data)
            break
          }
          const partial = PoolStateUpdateSchema.safeParse(f.d)
          if (partial.success) updates.push(partial.data)
          else console.warn('[stream] dropped a malformed pools.update', partial.error.issues)
          break
        }
        case 'head': {
          const r = HeadDataSchema.safeParse(f.d)
          if (r.success) store.recordHead(r.data.chainId, r.data.block)
          break
        }
        case 'rpc.health': {
          const r = EndpointStatusSchema.array().safeParse(f.d)
          if (r.success) endpoints = r.data
          else console.warn('[stream] dropped a malformed rpc.health', r.error.issues)
          break
        }
        case 'watch.hits': {
          const r = WatchHitDataSchema.safeParse(f.d)
          if (r.success) hits.push(r.data)
          break
        }
        default:
          break
      }
    }

    if (newPools.length) applyNewPools(qc, newPools)
    for (const u of updates) applyUpdate(u)
    if (endpoints) qc.setQueryData<RpcHealth>(qk.rpcHealth, (prev) => setRpcEndpoints(prev, endpoints!))
    if (hits.length) applyHits(hits)
  }

  /** Fade a row out of the feed, then drop it from the cache. */
  function fadeOut(chainId: string, address: string) {
    const k = poolKey(chainId, address)
    leaving.value = new Set(leaving.value).add(k)
    window.setTimeout(() => {
      qc.setQueryData<FeedData>(qk.feed, (old) => removePool(old, chainId, address))
      const next = new Set(leaving.value)
      next.delete(k)
      leaving.value = next
    }, 450)
  }

  function applyUpdate(u: Pool | PoolStateUpdate) {
    const k = poolKey(u.chainId, u.address)
    const full = 'pair' in u

    if (!full && u.favoritedAt !== undefined) {
      applyFavoriteChange(qc, u.chainId, u.address, u.favoritedAt)
      return
    }
    if (!full && u.removed) {
      // Failed the feed's liquidity and volume screen. Favorites are exempt on
      // the backend, so only the feed row goes.
      fadeOut(u.chainId, u.address)
      return
    }

    if (u.state === 'reorged') {
      qc.setQueryData<FeedData>(qk.feed, (old) => patchPool(old, u))
      const pool = qc.getQueryData<Pool>(qk.pool(u.chainId, u.address))
      toasts.once('reorg:' + k, `${pool?.pair ?? 'A pool'} was reorged out of the chain and removed from the feed.`, 'warn')
      qc.setQueryData<Pool[]>(qk.favorites, (old) => dropFavorite(old, u.chainId, u.address))
      fadeOut(u.chainId, u.address)
    } else {
      qc.setQueryData<FeedData>(qk.feed, (old) => patchPool(old, u))
      // Whether a pair is starred is the favorites list's call, not the frame's.
      const { favoritedAt: _ignored, ...figures } = u as Pool
      qc.setQueryData<Pool[]>(qk.favorites, (old) => patchFavorite(old, figures))
      qc.setQueriesData<Pool[]>({ queryKey: ['pools', 'search'] }, (old) => patchFavorite(old, figures))
    }
    qc.setQueryData<Pool>(qk.pool(u.chainId, u.address), (old) => (old ? ({ ...old, ...u } as Pool) : full ? u : old))
  }

  function applyHits(hits: WatchHitData[]) {
    const tokenHits = hits.filter((h) => h.kind === 'token')
    const topicHits = hits.filter((h) => h.kind === 'topic')
    if (tokenHits.length) {
      qc.setQueriesData<TokenWatch[]>({ queryKey: ['watch', 'tokens'] }, (old) =>
        old?.map((w) => {
          const mine = tokenHits.filter((h) => h.watchId === w.id)
          if (!mine.length) return w
          const existing = new Set(w.resolvedHits.map((h) => poolKey(h.chainId, h.pool)))
          const added = mine
            .filter((h) => !existing.has(poolKey(h.chainId, h.pool)))
            .map((h) => ({ chainId: h.chainId, pool: h.pool, pair: h.pair, dex: h.dex, seenAt: h.at }))
          return added.length ? { ...w, resolvedHits: [...added, ...w.resolvedHits] } : w
        }),
      )
      for (const h of tokenHits) toasts.once(`hit:${h.watchId}:${h.chainId}:${h.pool}`, `New pair for ${h.name}: ${h.pair} on ${h.dex}`, 'accent')
    }
    if (topicHits.length) {
      qc.setQueriesData<{ data: TopicWatch[]; limit?: number }>({ queryKey: ['watch', 'topics'] }, (old) =>
        old
          ? {
              ...old,
              data: old.data.map((w) => {
                const mine = topicHits.filter((h) => h.topic0.toLowerCase() === w.topic0.toLowerCase())
                const last = mine[mine.length - 1]
                if (!last) return w
                return {
                  ...w,
                  matches24h: w.matches24h + mine.length,
                  lastMatchAt: last.at,
                  lastTx: last.txHash,
                  lastChainId: last.chainId,
                }
              }),
            }
          : old,
      )
    }
  }

  // ------------------------------------------------------------ lifecycle

  const onVisible = () => {
    // Coming back to a tab whose socket died while throttled: do not wait out the backoff.
    if (!document.hidden && !ws && !stopped) {
      window.clearTimeout(reconnectTimer)
      open()
    }
  }
  document.addEventListener('visibilitychange', onVisible)
  window.addEventListener('online', onVisible)

  open()

  return {
    stop() {
      stopped = true
      detach()
      stopPolling()
      window.clearTimeout(reconnectTimer)
      document.removeEventListener('visibilitychange', onVisible)
      window.removeEventListener('online', onVisible)
      singleton = null
    },
    reconnectNow() {
      attempt = 0
      window.clearTimeout(reconnectTimer)
      detach()
      open()
    },
  }
}
