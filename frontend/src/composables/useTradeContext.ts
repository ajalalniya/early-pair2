import { computed, type Ref } from 'vue'
import { parseUnits } from 'viem'
import { chainMeta, explorerTx, orientPool } from '@/lib/chains'
import { isGated } from '@/lib/risk'
import { useChainLookup } from '@/queries'
import { useSettings } from '@/stores/settings'
import { useWallet } from '@/stores/wallet'
import type { Pool, Token } from '@/types/api'

/** Base units from a user-typed decimal, or null when it is not a positive amount. */
export function toBaseUnits(text: string, token: Token | null): string | null {
  if (!token) return null
  const t = text.trim().replace(/,/g, '')
  if (!/^\d*\.?\d+$|^\d+\.$/.test(t)) return null
  try {
    const v = parseUnits(t.endsWith('.') ? t.slice(0, -1) : t, token.decimals)
    return v > 0n ? v.toString() : null
  } catch {
    return null
  }
}

/** Shared pieces every trade tab needs: gate, chain ids, tokens, explorer links. */
export function useTradeContext(pool: Ref<Pool>) {
  const settings = useSettings()
  const wallet = useWallet()
  const chains = useChainLookup()

  const info = computed(() => chains.value.get(pool.value.chainId))
  const numericChainId = computed(() => chainMeta(pool.value.chainId, info.value).numericId)
  const nativeSymbol = computed(() => info.value?.nativeSymbol ?? '')
  const gated = computed(() => isGated(pool.value.risk?.score, settings.gateRiskyTrades, settings.riskGateThreshold))
  /** Sent to the backend so its gate honours the user's own setting. */
  const riskThreshold = computed(() => (settings.gateRiskyTrades ? settings.riskGateThreshold : 101))

  // The traded token and what pays for it, by the chain's quote assets — never
  // by slot order, which puts native ETH in slot 0 of most v4 pairs.
  const sides = computed(() => orientPool(pool.value, info.value))
  const token = computed(() => sides.value.base)
  const quoteToken = computed(() => sides.value.quote)
  // A pair priced in the wrapped native token can be paid in the native one.
  const quoteIsWrappedNative = computed(() => {
    const wrapped = info.value?.wrappedNative?.toLowerCase()
    return !!wrapped && quoteToken.value?.address.toLowerCase() === wrapped
  })
  const tokenSymbol = computed(() => token.value?.symbol || pool.value.pair.split(' / ')[0] || '?')
  const quoteSymbol = computed(() => quoteToken.value?.symbol || pool.value.pair.split(' / ')[1] || '?')

  const txUrl = (hash: string | null) => (hash ? explorerTx(pool.value.chainId, hash, info.value) : null)

  return {
    settings,
    wallet,
    info,
    numericChainId,
    nativeSymbol,
    gated,
    riskThreshold,
    token,
    quoteToken,
    tokenSymbol,
    quoteSymbol,
    quoteIsWrappedNative,
    txUrl,
  }
}
