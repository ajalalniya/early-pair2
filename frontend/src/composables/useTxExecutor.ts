/**
 * Signs backend-built transaction plans, never anything else.
 *
 * For each step, in order: switch the wallet to the right chain, simulate the
 * exact payload with `eth_call` from the user's address, and only then ask the
 * wallet to sign. A simulation revert stops the plan and surfaces the reason
 * instead of letting the wallet fail opaquely. Approvals are mined before the
 * next step is simulated, since a swap simulated without its allowance reverts
 * for a reason that has nothing to do with the trade.
 */
import { ref } from 'vue'
import { call, sendTransaction, switchChain, waitForTransactionReceipt } from '@wagmi/core'
import { BaseError, isAddress, isHex, type Hex } from 'viem'
import { api, errorMessage } from '@/lib/api'
import { useWallet, walletErrorMessage } from '@/stores/wallet'
import type { UnsignedTx } from '@/types/api'

export type TxPhase = 'idle' | 'building' | 'simulating' | 'signing' | 'confirming' | 'done' | 'error'

export class SimulationRevertError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'SimulationRevertError'
  }
}

export function revertReason(err: unknown): string {
  if (err instanceof BaseError) {
    const inner = err.walk((e) => typeof (e as { details?: unknown }).details === 'string') as { details?: string } | null
    const details = inner?.details ?? (err as { details?: string }).details
    if (details) return details.replace(/^execution reverted:?\s*/i, '') || 'execution reverted'
    return err.shortMessage
  }
  return errorMessage(err)
}

export function useTxExecutor() {
  const wallet = useWallet()
  const phase = ref<TxPhase>('idle')
  const stepLabel = ref('')
  const error = ref<string | null>(null)
  const txHash = ref<Hex | null>(null)

  function reset() {
    phase.value = 'idle'
    stepLabel.value = ''
    error.value = null
    txHash.value = null
  }

  async function execute(steps: UnsignedTx[], numericChainId: number): Promise<Hex | null> {
    const cfg = await wallet.init()
    const account = wallet.address
    if (!account) throw new Error('Connect a wallet first.')

    let last: Hex | null = null
    for (const s of steps) {
      if (!isAddress(s.to) || !isHex(s.data)) throw new Error('The transaction plan is malformed.')
      stepLabel.value = s.label

      if (wallet.chainId !== numericChainId) {
        await switchChain(cfg, { chainId: numericChainId as (typeof cfg.chains)[number]['id'] })
      }

      phase.value = 'simulating'
      const value = BigInt(s.value || '0')
      try {
        await call(cfg, { account, to: s.to, data: s.data, value, chainId: numericChainId as (typeof cfg.chains)[number]['id'] })
      } catch (err) {
        throw new SimulationRevertError(`Simulation reverted: ${revertReason(err)}`)
      }

      phase.value = 'signing'
      const hash = await sendTransaction(cfg, {
        account,
        to: s.to,
        data: s.data,
        value,
        gas: s.gas ? BigInt(s.gas) : undefined,
        chainId: numericChainId as (typeof cfg.chains)[number]['id'],
      })
      txHash.value = hash
      last = hash

      phase.value = 'confirming'
      const receipt = await waitForTransactionReceipt(cfg, { hash, chainId: numericChainId as (typeof cfg.chains)[number]['id'] })
      if (receipt.status !== 'success') throw new Error(`${s.label} reverted on-chain.`)
    }
    return last
  }

  /** Build a plan on the backend, then execute it. */
  async function buildAndExecute(body: Record<string, unknown>, numericChainId: number | undefined): Promise<Hex | null> {
    reset()
    try {
      if (!numericChainId) throw new Error('This chain cannot be signed for from this wallet.')
      phase.value = 'building'
      const plan = await api.buildTx(body)
      if (Date.parse(plan.expiresAt) <= Date.now()) throw new Error('The transaction plan expired before signing. Try again.')
      const hash = await execute(plan.steps, numericChainId)
      phase.value = 'done'
      return hash
    } catch (err) {
      phase.value = 'error'
      error.value = err instanceof SimulationRevertError ? err.message : walletErrorMessage(err)
      return null
    }
  }

  /**
   * Sign only the approvals a later trade will need, so that trade is a single
   * transaction when the moment comes. Returns whether the token is now
   * approved; an already-approved token signs nothing.
   */
  async function approveAhead(
    body: { chainId: string; pool: string; side?: 'buy' | 'sell' },
    numericChainId: number | undefined,
  ): Promise<boolean> {
    reset()
    try {
      if (!numericChainId) throw new Error('This chain cannot be signed for from this wallet.')
      if (!wallet.address) throw new Error('Connect a wallet first.')
      phase.value = 'building'
      const plan = await api.approveAhead({ ...body, from: wallet.address })
      if (plan.steps.length) await execute(plan.steps, numericChainId)
      phase.value = 'done'
      return true
    } catch (err) {
      phase.value = 'error'
      error.value = err instanceof SimulationRevertError ? err.message : walletErrorMessage(err)
      return false
    }
  }

  return { phase, stepLabel, error, txHash, reset, execute, buildAndExecute, approveAhead }
}
