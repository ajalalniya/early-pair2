<script setup lang="ts">
import { computed, ref, toRef, watch } from 'vue'
import { useQuote } from '@/composables/useQuote'
import { toBaseUnits, useTradeContext } from '@/composables/useTradeContext'
import { useTxExecutor } from '@/composables/useTxExecutor'
import { ApiError, api } from '@/lib/api'
import { DASH, bps, money, units } from '@/lib/format'
import type { Pool, QuoteLeg, ZapQuoteRequest } from '@/types/api'
import LegList from '@/components/ui/LegList.vue'
import TxStatus from './TxStatus.vue'

const props = defineProps<{ pool: Pool; active: boolean }>()
const pool = toRef(props, 'pool')
const ctx = useTradeContext(pool)
const { wallet, settings } = ctx

const amount = ref('')
const split = ref(50)

const amountIn = computed(() => toBaseUnits(amount.value, ctx.quoteToken.value))
const amountInvalid = computed(() => amount.value.trim() !== '' && amountIn.value === null)

const request = computed<ZapQuoteRequest | null>(() =>
  amountIn.value
    ? {
        chainId: props.pool.chainId,
        pool: props.pool.address,
        amountIn: amountIn.value,
        splitBps: split.value * 100,
        slippageBps: settings.slippageBps,
      }
    : null,
)

const tx = useTxExecutor()
const zapBusy = computed(() => ['building', 'simulating', 'signing', 'confirming'].includes(tx.phase.value))

const q = useQuote({
  request,
  active: computed(() => props.active && !ctx.gated.value && !zapBusy.value),
  fetch: (req, signal) => api.quoteZap(req, signal),
})
watch([amount, split], () => {
  if (tx.phase.value === 'done' || tx.phase.value === 'error') tx.reset()
})

/** Leg labels are structural; their values only ever come from the backend plan. */
const legs = computed<QuoteLeg[]>(
  () =>
    q.quote.value?.legs ?? [
      { step: '1', label: `Swap ${ctx.quoteSymbol.value} → ${ctx.tokenSymbol.value}`, value: DASH },
      { step: '2', label: 'Deposit pair', value: DASH },
      { step: '3', label: 'Mint LP position', value: DASH },
    ],
)

const qq = computed(() => q.quote.value)
const feeApr = computed(() => {
  const fee = qq.value?.feeTierBps ?? props.pool.feeTierBps
  const apr = qq.value?.estAprPct
  return `${fee !== null && fee !== undefined ? bps(fee) : DASH} · ${apr !== null && apr !== undefined ? apr.toFixed(1) + ' %' : DASH}`
})
const share = computed(() => (qq.value?.poolSharePctAfter != null ? qq.value.poolSharePctAfter.toFixed(2) + ' %' : DASH))
const impact = computed(() => (qq.value ? `${bps(qq.value.priceImpactBps)} + ${bps(qq.value.lpSlipBps)}` : DASH))
const gas = computed(() => (qq.value?.estGasUsd != null ? '~' + money(qq.value.estGasUsd) : DASH))
const minReceived = computed(() => (qq.value ? `${units(qq.value.minReceived, ctx.token.value?.decimals)} ${ctx.tokenSymbol.value}` : DASH))

const busy = computed(() => ['building', 'simulating', 'signing', 'confirming'].includes(tx.phase.value))
const blocked = ctx.gated

const label = computed(() => {
  if (blocked.value) return 'Blocked — high risk pool'
  if (!wallet.connected) return 'Connect wallet'
  if (tx.phase.value === 'done') return 'Zapped'
  if (busy.value) return 'Confirm in wallet…'
  return 'Buy + add liquidity'
})

const unavailable = computed(() => q.error.value instanceof ApiError && q.error.value.notImplemented)

const disabled = computed(() => {
  if (blocked.value) return true
  if (!wallet.connected) return false
  return busy.value || tx.phase.value === 'done' || !q.ready.value || (qq.value?.gated ?? false)
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
      kind: 'zap',
      chainId: props.pool.chainId,
      pool: props.pool.address,
      amountIn: quote.amountIn,
      splitBps: split.value * 100,
      slippageBps: settings.slippageBps,
      minReceived: quote.minReceived,
      from: wallet.address,
      riskThreshold: ctx.riskThreshold.value,
    },
    ctx.numericChainId.value,
  )
  if (hash) bought.value = units(quote.amountOut, ctx.token.value?.decimals)
}
const bought = ref('')
</script>

<template>
  <div class="tab-body">
    <div class="notice" data-tone="accent">One transaction: swap half, then deposit both sides. Reverts entirely if any leg fails.</div>

    <label class="box field">
      <span class="field-head mono"><span>Zap in</span><span>{{ ctx.quoteSymbol.value }}</span></span>
      <input v-model="amount" class="big-input" inputmode="decimal" placeholder="0.0" autocomplete="off" :aria-invalid="amountInvalid" data-testid="zap-amount" />
    </label>

    <div class="split">
      <div class="split-head mono">
        <label for="zap-split">SPLIT</label>
        <span>{{ split }}% swap / {{ 100 - split }}% paired</span>
      </div>
      <input id="zap-split" v-model.number="split" type="range" min="10" max="90" step="5" />
      <div class="ends mono"><span>More LP depth</span><span>More token exposure</span></div>
    </div>

    <LegList :legs="legs" />

    <div class="kv">
      <div><span>Fee tier / est. APR</span><span>{{ feeApr }}</span></div>
      <div><span>Pool share after</span><span>{{ share }}</span></div>
      <div><span>Swap impact + LP slip</span><span>{{ impact }}</span></div>
      <div><span>Min received</span><span>{{ minReceived }}</span></div>
      <div><span>Est. gas (bundled)</span><span>{{ gas }}</span></div>
    </div>

    <div v-if="unavailable" class="notice" data-tone="muted" role="status">Zap is not available yet: {{ q.error.value?.message }}</div>
    <div v-else-if="q.error.value && !blocked" class="notice" data-tone="warn" role="alert">{{ q.error.value.message }}</div>

    <button class="action" type="button" :data-tone="blocked ? 'blocked' : busy ? 'busy' : 'buy'" :disabled="disabled" data-testid="zap-action" @click="onAction">
      {{ label }}
    </button>
    <TxStatus :tx="tx" :receipt="`Bought ${bought} ${ctx.tokenSymbol.value} and minted LP · 1 tx`" :explorer="ctx.txUrl(tx.txHash.value)" />
  </div>
</template>

<style scoped>
.split {
  display: flex;
  flex-direction: column;
  gap: 5px;
}
.split-head {
  display: flex;
  justify-content: space-between;
  font-size: 11px;
  color: var(--fg-muted);
  letter-spacing: 0.05em;
}
.ends {
  display: flex;
  justify-content: space-between;
  font-size: 11px;
  color: oklch(0.6 0.01 265);
}
</style>
