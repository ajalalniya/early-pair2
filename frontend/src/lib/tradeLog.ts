/**
 * Reports confirmed swaps so they appear in the portfolio.
 *
 * The backend verifies each report against the mined receipt — the signer, the
 * router, the pool's own Swap event — and builds holdings and their cost basis
 * from what it verified. A report can fail for reasons that pass: the backend's
 * RPC endpoint has not seen the block yet, or the network dropped. Those are
 * queued in localStorage and retried, including after a reload, so a token
 * bought here still reaches the portfolio. A report the backend refuses outright
 * (not a swap in that pool) is dropped: retrying cannot change the chain.
 */
import type { QueryClient } from '@tanstack/vue-query'
import { ApiError, api } from './api'

const KEY = 'earlypair.pendingTrades.v1'
const MAX_AGE_MS = 7 * 24 * 60 * 60 * 1000
const RETRY_DELAYS_MS = [3_000, 10_000, 30_000, 120_000]

export interface TradeReport {
  chainId: string
  pool: string
  txHash: string
}

interface Pending extends TradeReport {
  at: number
  tries: number
}

function load(): Pending[] {
  try {
    const v = JSON.parse(localStorage.getItem(KEY) || '[]') as Pending[]
    return Array.isArray(v) ? v : []
  } catch {
    return []
  }
}

function save(list: Pending[]) {
  try {
    if (list.length) localStorage.setItem(KEY, JSON.stringify(list))
    else localStorage.removeItem(KEY)
  } catch {
    /* private mode: retries last only as long as the tab */
  }
}

const same = (a: TradeReport, b: TradeReport) => a.chainId === b.chainId && a.txHash.toLowerCase() === b.txHash.toLowerCase()

/** Worth retrying: the chain or the network may answer differently later. */
export function retryable(err: unknown): boolean {
  if (!(err instanceof ApiError)) return true
  if (err.code === 'not_mined') return true
  return err.status === 0 || err.status >= 500
}

let timer: number | undefined

function schedule(qc: QueryClient, delay: number) {
  window.clearTimeout(timer)
  timer = window.setTimeout(() => void flushPendingTrades(qc), delay)
}

async function send(qc: QueryClient, report: TradeReport): Promise<'recorded' | 'retry' | 'dropped'> {
  try {
    await api.recordTrade(report)
    void qc.invalidateQueries({ queryKey: ['portfolio', 'holdings'] })
    return 'recorded'
  } catch (err) {
    return retryable(err) ? 'retry' : 'dropped'
  }
}

/**
 * Remember a swap the moment the wallet has signed it, before it confirms, so a
 * tab closed while waiting still reports it on the next visit.
 */
export function rememberTrade(report: TradeReport): void {
  const list = load()
  if (!list.some((p) => same(p, report))) list.push({ ...report, at: Date.now(), tries: 0 })
  save(list)
}

/** Report a confirmed swap; on a passing failure it stays queued and is retried. */
export async function recordTrade(qc: QueryClient, report: TradeReport): Promise<void> {
  const outcome = await send(qc, report)
  const list = load().filter((p) => !same(p, report))
  if (outcome === 'retry') {
    list.push({ ...report, at: Date.now(), tries: 1 })
    schedule(qc, RETRY_DELAYS_MS[0]!)
  }
  save(list)
}

/** Retry queued reports. Called on start-up and on a timer after a failure. */
export async function flushPendingTrades(qc: QueryClient): Promise<void> {
  const now = Date.now()
  const list = load().filter((p) => now - p.at < MAX_AGE_MS)
  const keep: Pending[] = []
  for (const p of list) {
    const outcome = await send(qc, p)
    if (outcome === 'retry') keep.push({ ...p, tries: p.tries + 1 })
  }
  save(keep)
  if (keep.length) {
    const tries = Math.min(...keep.map((p) => p.tries))
    schedule(qc, RETRY_DELAYS_MS[Math.min(tries, RETRY_DELAYS_MS.length - 1)]!)
  }
}
