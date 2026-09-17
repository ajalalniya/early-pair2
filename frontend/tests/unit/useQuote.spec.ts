import { effectScope, nextTick, ref } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { REQUOTE_MS, useQuote } from '@/composables/useQuote'
import type { Quote } from '@/types/api'

const makeQuote = (amountOut: string, ttlMs = 30_000) =>
  ({
    legs: [], amountIn: '1', amountOut, minReceived: '1', priceImpactBps: 0, rate: '', estGas: 0, gasPriceWei: '',
    estGasNative: '', estGasUsd: null, expiresAt: new Date(Date.now() + ttlMs).toISOString(), gated: false,
    feeTierBps: null, estAprPct: null, poolSharePctAfter: null, lpSlipBps: null, feesClaimedUsd: null, amountOutUsd: null, remainingUsd: null,
  }) as Quote

interface Call {
  signal: AbortSignal
  resolve: (q: Quote) => void
  reject: (e: Error) => void
}

function setup() {
  const calls: Call[] = []
  const fetch = vi.fn(
    (_req: { pool: string; amount: string }, signal: AbortSignal) =>
      new Promise<Quote>((resolve, reject) => calls.push({ signal, resolve, reject })),
  )
  const request = ref<{ pool: string; amount: string } | null>({ pool: '0xp', amount: '1' })
  const active = ref(true)
  const scope = effectScope()
  const q = scope.run(() => useQuote({ request, active, fetch }))!
  return { calls, fetch, request, active, scope, q }
}

describe('useQuote', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('does not cancel or re-fetch when an identical request is re-created', async () => {
    const { calls, fetch, request, q, scope } = setup()
    await vi.advanceTimersByTimeAsync(350)
    expect(fetch).toHaveBeenCalledTimes(1)

    // A stream update re-creates the request mid-flight with the same values.
    request.value = { pool: '0xp', amount: '1' }
    await nextTick()
    expect(calls[0]!.signal.aborted).toBe(false)

    calls[0]!.resolve(makeQuote('100'))
    await vi.advanceTimersByTimeAsync(0)
    expect(q.ready.value).toBe(true)

    request.value = { pool: '0xp', amount: '1' }
    await vi.advanceTimersByTimeAsync(1_000)
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(q.ready.value).toBe(true)
    scope.stop()
  })

  it('keeps the quote usable during a refresh, and pauses between requests', async () => {
    const { calls, fetch, q, scope } = setup()
    await vi.advanceTimersByTimeAsync(350)
    calls[0]!.resolve(makeQuote('100'))
    await vi.advanceTimersByTimeAsync(0)

    await vi.advanceTimersByTimeAsync(REQUOTE_MS - 1)
    expect(fetch).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(fetch).toHaveBeenCalledTimes(2)

    // The refresh is in flight, and the held quote can still be acted on.
    expect(q.inFlight.value).toBe(true)
    expect(q.ready.value).toBe(true)
    expect(q.refreshing.value).toBe(true)

    // A slow refresh does not stack another request behind it.
    await vi.advanceTimersByTimeAsync(REQUOTE_MS * 2)
    expect(fetch).toHaveBeenCalledTimes(2)
    calls[1]!.resolve(makeQuote('101'))
    await vi.advanceTimersByTimeAsync(0)
    expect(q.quote.value?.amountOut).toBe('101')
    scope.stop()
  })

  it('re-quotes from scratch when the inputs change', async () => {
    const { calls, fetch, request, q, scope } = setup()
    await vi.advanceTimersByTimeAsync(350)
    calls[0]!.resolve(makeQuote('100'))
    await vi.advanceTimersByTimeAsync(0)

    request.value = { pool: '0xp', amount: '2' }
    await nextTick()
    expect(q.ready.value).toBe(false) // a quote for 1 is not a quote for 2
    await vi.advanceTimersByTimeAsync(350)
    expect(fetch).toHaveBeenCalledTimes(2)
    scope.stop()
  })

  it('keeps a still-valid quote when a refresh fails', async () => {
    const { calls, q, scope } = setup()
    await vi.advanceTimersByTimeAsync(350)
    calls[0]!.resolve(makeQuote('100'))
    await vi.advanceTimersByTimeAsync(REQUOTE_MS)
    calls[1]!.reject(new Error('rpc hiccup'))
    await vi.advanceTimersByTimeAsync(0)
    expect(q.ready.value).toBe(true)
    expect(q.error.value).toBeNull()
    scope.stop()
  })

  it('stops refreshing while inactive, and resumes with the held quote', async () => {
    const { calls, fetch, active, q, scope } = setup()
    await vi.advanceTimersByTimeAsync(350)
    calls[0]!.resolve(makeQuote('100'))
    await vi.advanceTimersByTimeAsync(0)

    active.value = false // e.g. a transaction is being signed
    await vi.advanceTimersByTimeAsync(REQUOTE_MS * 3)
    expect(fetch).toHaveBeenCalledTimes(1)

    active.value = true
    await nextTick()
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(q.quote.value).not.toBeNull()
    scope.stop()
  })
})
