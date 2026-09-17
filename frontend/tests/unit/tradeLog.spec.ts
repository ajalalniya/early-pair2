import { QueryClient } from '@tanstack/vue-query'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api } from '@/lib/api'
import { flushPendingTrades, recordTrade, rememberTrade, retryable } from '@/lib/tradeLog'

const KEY = 'earlypair.pendingTrades.v1'
const report = { chainId: 'rbn', pool: '0xpool', txHash: '0x' + 'ab'.repeat(32) }
const queued = () => JSON.parse(localStorage.getItem(KEY) || '[]') as { txHash: string }[]

describe('trade reports', () => {
  beforeEach(() => {
    localStorage.clear()
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.restoreAllMocks()
    vi.useRealTimers()
  })

  it('retries what may pass and drops what the chain has already answered', () => {
    expect(retryable(new ApiError(404, 'not_mined', ''))).toBe(true)
    expect(retryable(new ApiError(0, 'network', ''))).toBe(true)
    expect(retryable(new ApiError(502, 'receipt_unavailable', ''))).toBe(true)
    expect(retryable(new ApiError(422, 'not_a_trade', ''))).toBe(false)
    expect(retryable(new ApiError(404, 'not_found', ''))).toBe(false)
  })

  it('keeps a signed trade until it is recorded', async () => {
    rememberTrade(report)
    expect(queued()).toHaveLength(1)

    const spy = vi.spyOn(api, 'recordTrade').mockRejectedValueOnce(new ApiError(404, 'not_mined', 'no receipt yet'))
    const qc = new QueryClient()
    await recordTrade(qc, report)
    expect(queued()).toHaveLength(1) // not mined yet: still queued, once

    spy.mockResolvedValueOnce({ recorded: true, side: 'buy', token: '0xt', tokenAmount: '1' })
    await flushPendingTrades(qc)
    expect(queued()).toHaveLength(0)
    expect(spy).toHaveBeenCalledTimes(2)
  })

  it('drops a report the backend refuses', async () => {
    rememberTrade(report)
    vi.spyOn(api, 'recordTrade').mockRejectedValueOnce(new ApiError(422, 'not_a_trade', 'did not swap in this pool'))
    await recordTrade(new QueryClient(), report)
    expect(queued()).toHaveLength(0)
  })
})
