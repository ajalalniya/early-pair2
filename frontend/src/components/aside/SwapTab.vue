<script setup lang="ts">
import { computed, ref, toRef, watch } from 'vue'
import { useQueryClient } from '@tanstack/vue-query'
import { useRoute } from 'vue-router'
import { useQuote } from '@/composables/useQuote'
import { toBaseUnits, useTradeContext } from '@/composables/useTradeContext'
import { useTxExecutor } from '@/composables/useTxExecutor'
import { api } from '@/lib/api'
import { verdict } from '@/lib/risk'
import { recordTrade, rememberTrade } from '@/lib/tradeLog'
import { DASH, bps, money, trimDecimal, units } from '@/lib/format'
import { formatUnits } from 'viem'
import type { Pool, SwapQuoteRequest } from '@/types/api'
import TxStatus from './TxStatus.vue'

const props = defineProps<{
  pool: Pool
  active: boolean
  /** Asks the tab to switch side; `n` changes on every request, so repeating one still applies. */
  sideRequest?: { side: 'buy' | 'sell'; n: number }
  /** The wallet's balance of the traded token, in base units, when known (the portfolio passes it). */
  held?: string | null
}>()
const pool = toRef(props, 'pool')
const ctx = useTradeContext(pool)
const { wallet, settings } = ctx

const route = useRoute()
const side = ref<'buy' | 'sell'>(props.sideRequest?.side ?? (route.query.side === 'sell' ? 'sell' : 'buy'))
watch(
  () => props.sideRequest?.n,
  () => {
    if (props.sideRequest) side.value = props.sideRequest.side
  },
)
const amount = ref('')
const slippageText = ref((settings.slippageBps / 100).toFixed(1))

const slippageBps = computed(() => {
  const v = parseFloat(slippageText.value)
  return Number.isFinite(v) && v > 0 && v <= 50 ? Math.round(v * 100) : null
})
watch(slippageBps, (v) => {
  if (v !== null) settings.slippageBps = v
})

// Buying a pair priced in WETH can be paid in ETH: the router wraps it inside
// the swap. The choice is remembered; ETH is the default, since that is what a
// wallet usually holds.
const canPayNative = computed(() => side.value === 'buy' && ctx.quoteIsWrappedNative.value && !!ctx.nativeSymbol.value)
const payNative = computed(() => canPayNative.value && settings.payWithNative)

const payToken = computed(() => (side.value === 'buy' ? ctx.quoteToken.value : ctx.token.value))
const getToken = computed(() => (side.value === 'buy' ? ctx.token.value : ctx.quoteToken.value))
const paySymbol = computed(() =>
  side.value === 'buy' ? (payNative.value ? ctx.nativeSymbol.value : ctx.quoteSymbol.value) : ctx.tokenSymbol.value,
)
const getSymbol = computed(() => (side.value === 'buy' ? ctx.tokenSymbol.value : ctx.quoteSymbol.value))
const amountIn = computed(() => toBaseUnits(amount.value, payToken.value))
const amountInvalid = computed(() => amount.value.trim() !== '' && amountIn.value === null)

// The risk gate stops a buy the scorer judged dangerous. It is a guard, not a
// verdict the user cannot overrule: accepting it here unlocks this pool's buys,
// for this pool only, and only until the panel moves to another pair. Sells are
// never gated — a position must always be exitable.
const gatedPool = computed(() => side.value === 'buy' && ctx.gated.value)
const riskAccepted = ref(false)
watch(
  () => props.pool.address,
  () => (riskAccepted.value = false),
)
const blocked = computed(() => gatedPool.value && !riskAccepted.value)
// 101 is above every possible score, so the backend gates nothing for a request
// the user has explicitly accepted the risk on.
const riskThreshold = computed(() => (riskAccepted.value ? 101 : ctx.riskThreshold.value))
// The same words the fraud card shows, so the warning says what is wrong with
// this pool rather than repeating a generic caution.
const gateReason = computed(() => verdict(props.pool, settings.riskGateThreshold))

// Selling from the portfolio: what the wallet holds, and one click to sell it all.
const heldAmount = computed(() => {
  const t = ctx.token.value
  if (!props.held || !t) return null
  try {
    return formatUnits(BigInt(props.held), t.decimals)
  } catch {
    return null
  }
})
function sellMax() {
  if (heldAmount.value) amount.value = heldAmount.value
}

const request = computed<SwapQuoteRequest | null>(() => {
  if (!amountIn.value || slippageBps.value === null) return null
  return {
    chainId: props.pool.chainId,
    pool: props.pool.address,
    side: side.value,
    amountIn: amountIn.value,
    slippageBps: slippageBps.value,
    riskThreshold: riskThreshold.value,
    ...(payNative.value ? { payNative: true } : {}),
  }
})

const tx = useTxExecutor()
const qc = useQueryClient()
const busy = computed(() => ['building', 'simulating', 'signing', 'confirming'].includes(tx.phase.value))

// No refreshing while a transaction is being built, signed or confirmed: the
// quote it was built from stays on screen.
const q = useQuote({
  request,
  active: computed(() => props.active && !blocked.value && !busy.value),
  fetch: (req, signal) => api.quoteSwap(req, signal),
})
// Signed but not yet confirmed: remembered, so closing the tab does not lose it.
watch(tx.txHash, (hash) => {
  if (hash) rememberTrade({ chainId: props.pool.chainId, pool: props.pool.address, txHash: hash })
})
watch([side, amount], () => {
  if (tx.phase.value === 'done' || tx.phase.value === 'error') tx.reset()
})

const outAmount = computed(() => (q.quote.value ? units(q.quote.value.amountOut, getToken.value?.decimals) : DASH))
const minReceived = computed(() =>
  q.quote.value ? `${units(q.quote.value.minReceived, getToken.value?.decimals)} ${getSymbol.value}` : DASH,
)
const gas = computed(() => {
  const qq = q.quote.value
  if (!qq) return DASH
  // L2 gas is routinely cents; whole-dollar rounding would show it as "$0".
  if (qq.estGasUsd !== null) return qq.estGasUsd < 1 ? (qq.estGasUsd < 0.01 ? '<$0.01' : `~$${qq.estGasUsd.toFixed(2)}`) : '~' + money(qq.estGasUsd)
  return qq.estGasNative ? `${trimDecimal(qq.estGasNative, 6)} ${ctx.nativeSymbol.value}` : DASH
})

const label = computed(() => {
  if (blocked.value) return 'High risk — accept below to trade'
  if (!wallet.connected) return 'Connect wallet to trade'
  if (tx.phase.value === 'signing') return 'Confirm in wallet…'
  if (tx.phase.value === 'building') return 'Preparing transaction…'
  if (tx.phase.value === 'simulating') return 'Simulating…'
  if (tx.phase.value === 'confirming') return 'Waiting for confirmation…'
  if (riskAccepted.value && gatedPool.value) return `Buy ${ctx.tokenSymbol.value} anyway`
  return `${side.value === 'buy' ? 'Buy' : 'Sell'} ${ctx.tokenSymbol.value}`
})

const tone = computed(() => {
  if (blocked.value) return 'blocked'
  if (busy.value) return 'busy'
  // An accepted high-risk buy keeps the warning colour: it is still that trade.
  if (gatedPool.value) return 'risky'
  return side.value === 'sell' ? 'sell' : 'buy'
})

const disabled = computed(() => {
  if (blocked.value) return true
  if (!wallet.connected) return false
  if (busy.value || !q.ready.value) return true
  // The backend echoes the gate on the quote; an accepted risk clears it there too.
  return (q.quote.value?.gated ?? false) && side.value === 'buy' && !riskAccepted.value
})

const hint = computed(() => {
  if (blocked.value || !wallet.connected || busy.value) return ''
  if (!payToken.value || !getToken.value) return 'Token details are still being resolved for this pool.'
  if (amountInvalid.value) return 'Enter a valid amount.'
  if (slippageBps.value === null) return 'Slippage must be between 0.01 % and 50 %.'
  if (!amountIn.value) return ''
  if (q.inFlight.value && !q.ready.value) return 'Fetching a quote…'
  if (q.expired.value) return 'Quote expired — refreshing.'
  return ''
})

async function onAction() {
  if (blocked.value) return
  if (!wallet.connected) {
    wallet.openModal()
    return
  }
  const quote = q.quote.value
  if (!quote || !q.ready.value || !wallet.address) return
  const hash = await tx.buildAndExecute(
    {
      chainId: props.pool.chainId,
      pool: props.pool.address,
      side: side.value,
      amountIn: quote.amountIn,
      minReceived: quote.minReceived,
      from: wallet.address,
      riskThreshold: riskThreshold.value,
      ...(payNative.value ? { payNative: true } : {}),
    },
    ctx.numericChainId.value,
  )
  if (hash) {
    receiptAmount.value = outAmount.value
    // Buys and sells both reach the portfolio: the backend verifies the receipt.
    void recordTrade(qc, { chainId: props.pool.chainId, pool: props.pool.address, txHash: hash })
  }
}
const receiptAmount = ref('')
const receipt = computed(() =>
  side.value === 'buy' ? `Bought ${receiptAmount.value} ${getSymbol.value} · 1 tx` : `Sold for ${receiptAmount.value} ${getSymbol.value} · 1 tx`,
)
</script>

<template>
  <div class="tab-body">
    <div class="segmented" role="group" aria-label="Side">
      <button type="button" class="side-buy" :aria-pressed="side === 'buy'" @click="side = 'buy'">Buy</button>
      <button type="button" class="side-sell" :aria-pressed="side === 'sell'" @click="side = 'sell'">Sell</button>
    </div>

    <label class="field box">
      <span class="field-head mono">
        <span>{{ side === 'buy' ? 'You pay' : 'You sell' }}</span>
        <span v-if="canPayNative" class="pay-with" role="group" aria-label="Pay with">
          <button type="button" :aria-pressed="payNative" data-testid="pay-native" @click.prevent="settings.payWithNative = true">
            {{ ctx.nativeSymbol.value }}
          </button>
          <button type="button" :aria-pressed="!payNative" data-testid="pay-wrapped" @click.prevent="settings.payWithNative = false">
            {{ ctx.quoteSymbol.value }}
          </button>
        </span>
        <span v-else>{{ paySymbol }}</span>
      </span>
      <span v-if="side === 'sell' && heldAmount" class="held mono">
        <span data-testid="swap-held">You hold {{ trimDecimal(heldAmount, 4) }} {{ ctx.tokenSymbol.value }}</span>
        <button type="button" class="max" data-testid="swap-max" @click.prevent="sellMax">Max</button>
      </span>
      <input
        v-model="amount"
        class="big-input"
        inputmode="decimal"
        placeholder="0.0"
        autocomplete="off"
        :aria-invalid="amountInvalid"
        data-testid="swap-amount"
      />
    </label>

    <div class="box inset">
      <span class="field-head mono"><span>You receive</span><span>{{ getSymbol }}</span></span>
      <div class="out mono" data-testid="swap-out">{{ outAmount }}</div>
    </div>

    <div class="kv">
      <div><span>Rate</span><span>{{ q.quote.value?.rate || DASH }}</span></div>
      <div><span>Price impact</span><span>{{ q.quote.value ? bps(q.quote.value.priceImpactBps) : DASH }}</span></div>
      <div>
        <label for="slippage">Slippage</label>
        <span class="slip">
          <input id="slippage" v-model="slippageText" class="slip-input mono" inputmode="decimal" :aria-invalid="slippageBps === null" />
          %
        </span>
      </div>
      <div><span>Min received</span><span>{{ minReceived }}</span></div>
      <div><span>Est. gas</span><span>{{ gas }}</span></div>
    </div>

    <div v-if="q.error.value && !blocked" class="notice" data-tone="warn" role="alert">{{ q.error.value.message }}</div>

    <div v-if="gatedPool" class="risk-gate" data-testid="risk-gate">
      <p class="risk-why">{{ gateReason }}</p>
      <label class="risk-accept">
        <input v-model="riskAccepted" type="checkbox" data-testid="risk-accept" />
        <span>I understand the risk and want to trade this pool anyway.</span>
      </label>
    </div>

    <button class="action" type="button" :data-tone="tone" :disabled="disabled" data-testid="swap-action" @click="onAction">
      {{ label }}
    </button>
    <div v-if="hint" class="hint mono">{{ hint }}</div>
    <TxStatus :tx="tx" :receipt="receipt" :explorer="ctx.txUrl(tx.txHash.value)" />
  </div>
</template>

<style scoped>
.segmented > .side-buy[aria-pressed='true'] {
  background: oklch(0.34 0.09 168);
  color: oklch(0.9 0.13 168);
}
.segmented > .side-sell[aria-pressed='true'] {
  background: oklch(0.34 0.1 25);
  color: oklch(0.88 0.13 25);
}
.pay-with {
  display: inline-flex;
  gap: 2px;
  padding: 2px;
  border: 1px solid var(--border-soft);
  border-radius: 7px;
}
.pay-with > button {
  border: none;
  border-radius: 5px;
  background: transparent;
  padding: 1px 7px;
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--fg-muted);
  cursor: pointer;
}
.pay-with > button[aria-pressed='true'] {
  background: var(--chip-active);
  color: var(--fg);
}
.held {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  margin-top: 4px;
  font-size: 11px;
  color: var(--fg-muted);
}
.max {
  border: 1px solid var(--border);
  border-radius: 6px;
  background: transparent;
  padding: 1px 7px;
  font-size: 10.5px;
  font-weight: 600;
  letter-spacing: 0.04em;
  color: oklch(0.86 0.12 25);
  cursor: pointer;
}
.max:hover {
  border-color: oklch(0.5 0.1 25);
}
.out {
  margin-top: 6px;
  font-size: 24px;
  font-weight: 500;
  color: var(--fg-2);
  overflow-wrap: anywhere;
}
.slip {
  display: inline-flex;
  align-items: center;
  gap: 4px;
}
.slip-input {
  width: 46px;
  text-align: right;
  background: transparent;
  border: 1px solid var(--border);
  border-radius: 6px;
  padding: 2px 5px;
  font-size: 11.5px;
  color: oklch(0.86 0.01 265);
}
.slip-input[aria-invalid='true'] {
  border-color: var(--danger);
}
.hint {
  font-size: 11px;
  color: var(--fg-muted);
  text-align: center;
}

.action[data-tone='risky'] {
  background: oklch(0.36 0.1 40);
  color: oklch(0.92 0.12 60);
}
.risk-gate {
  border: 1px solid color-mix(in oklch, var(--risk-danger, oklch(0.72 0.16 25)) 45%, transparent);
  border-radius: 10px;
  padding: 10px 12px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.risk-why {
  margin: 0;
  font-size: 12px;
  line-height: 1.5;
  color: oklch(0.82 0.12 25);
}
.risk-accept {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  font-size: 12px;
  line-height: 1.4;
  cursor: pointer;
}
.risk-accept input {
  margin-top: 2px;
  accent-color: oklch(0.72 0.16 25);
}
</style>