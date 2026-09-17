<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import ModalShell from '@/components/ui/ModalShell.vue'
import LegList from '@/components/ui/LegList.vue'
import RiskChip from '@/components/ui/RiskChip.vue'
import TxStatus from '@/components/aside/TxStatus.vue'
import { useQuote } from '@/composables/useQuote'
import { useTxExecutor } from '@/composables/useTxExecutor'
import { ApiError, api } from '@/lib/api'
import { chainMeta, explorerTx } from '@/lib/chains'
import { DASH, bps, money, signedMoney, units } from '@/lib/format'
import { useChainLookup } from '@/queries'
import { useSettings } from '@/stores/settings'
import { useWallet } from '@/stores/wallet'
import type { ExitQuoteRequest, Position, QuoteLeg } from '@/types/api'

const props = defineProps<{ position: Position }>()
const emit = defineEmits<{ close: [] }>()

const wallet = useWallet()
const settings = useSettings()
const chains = useChainLookup()

const pct = ref(100)
const [tokA = '?', tokB = props.position.quoteSymbol] = props.position.pair.split(' / ')

const request = computed<ExitQuoteRequest>(() => ({
  chainId: props.position.chainId,
  positionId: props.position.id,
  pct: pct.value,
  slippageBps: settings.slippageBps,
}))

const q = useQuote({
  request,
  active: ref(true),
  fetch: (req, signal) => api.quoteExit(req, signal),
})
const tx = useTxExecutor()
watch(pct, () => {
  if (tx.phase.value === 'done' || tx.phase.value === 'error') tx.reset()
})

const qq = computed(() => q.quote.value)
const quote = computed(() => props.position.quoteSymbol)
const legs = computed<QuoteLeg[]>(
  () =>
    qq.value?.legs ?? [
      { step: '1', label: 'Burn LP · claim fees', value: DASH },
      { step: '2', label: `Withdraw ${tokA} + ${tokB}`, value: `${pct.value}%` },
      { step: '3', label: `Sell ${tokA} → ${tokB}`, value: DASH },
      { step: '4', label: 'Settle to wallet', value: quote.value },
    ],
)

const out = computed(() => (qq.value ? `${units(qq.value.amountOut, qq.value.quoteDecimals)} ${quote.value}` : DASH))
const outUsd = computed(() => (qq.value?.amountOutUsd != null ? '≈ ' + money(qq.value.amountOutUsd) : DASH))
const busy = computed(() => ['building', 'simulating', 'signing', 'confirming'].includes(tx.phase.value))

const label = computed(() => {
  if (!wallet.connected) return 'Connect wallet'
  if (tx.phase.value === 'done') return 'Settled'
  if (busy.value) return 'Confirm in wallet…'
  return `Remove + sell ${pct.value}%`
})

const unavailable = computed(() => q.error.value instanceof ApiError && q.error.value.notImplemented)
const receipt = ref('')

async function run() {
  if (!wallet.connected) {
    wallet.openModal()
    return
  }
  const quoteNow = q.quote.value
  if (!quoteNow || !q.ready.value || !wallet.address) return
  const hash = await tx.buildAndExecute(
    {
      kind: 'exit',
      chainId: props.position.chainId,
      positionId: props.position.id,
      pct: pct.value,
      slippageBps: settings.slippageBps,
      minReceived: quoteNow.minReceived,
      from: wallet.address,
    },
    chainMeta(props.position.chainId, chains.value.get(props.position.chainId)).numericId,
  )
  if (hash) receipt.value = `Removed ${pct.value}% and sold to ${out.value} · 1 tx`
}
</script>

<template>
  <ModalShell title="Remove + sell" :subtitle="`${position.pair} · ${position.dex}`" :width="420" @close="emit('close')">
    <div class="body">
      <div class="risk-row mono">
        <span>Pool risk</span>
        <RiskChip :risk="position.risk" />
      </div>

      <div class="notice" data-tone="danger">One transaction: burn LP, claim fees, sell the volatile side into {{ quote }}. Reverts entirely if any leg fails.</div>

      <div class="withdraw">
        <div class="field-head"><label for="exit-pct">WITHDRAW</label><span class="v">{{ pct }}% of position</span></div>
        <input id="exit-pct" v-model.number="pct" class="danger-range" type="range" min="10" max="100" step="5" />
        <div class="chip-row">
          <button v-for="p in [25, 50, 75, 100]" :key="p" type="button" class="preset-exit" :aria-pressed="pct === p" @click="pct = p">{{ p }}%</button>
        </div>
      </div>

      <LegList :legs="legs" />

      <div class="receive">
        <span class="caps">YOU RECEIVE</span>
        <span class="amounts">
          <span class="out mono">{{ out }}</span>
          <span class="usd mono">{{ outUsd }}</span>
        </span>
      </div>

      <div class="kv">
        <div><span>Fees claimed</span><span class="fees">{{ signedMoney(qq?.feesClaimedUsd) }}</span></div>
        <div><span>Sell impact</span><span>{{ qq ? bps(qq.priceImpactBps) : DASH }}</span></div>
        <div><span>Remaining position</span><span>{{ pct === 100 ? 'Closed' : money(qq?.remainingUsd) }}</span></div>
        <div><span>Min received</span><span>{{ qq ? `${units(qq.minReceived, qq.quoteDecimals)} ${quote}` : DASH }}</span></div>
        <div><span>Est. gas (bundled)</span><span>{{ qq?.estGasUsd != null ? '~' + money(qq.estGasUsd) : DASH }}</span></div>
      </div>

      <div v-if="unavailable" class="notice" data-tone="muted" role="status">Remove + sell is not available yet: {{ q.error.value?.message }}</div>
      <div v-else-if="q.error.value" class="notice" data-tone="warn" role="alert">{{ q.error.value.message }}</div>

      <button
        class="action"
        type="button"
        :data-tone="busy ? 'busy' : 'sell'"
        :disabled="wallet.connected && (busy || tx.phase.value === 'done' || !q.ready.value)"
        @click="run"
      >
        {{ label }}
      </button>
      <TxStatus :tx="tx" :receipt="receipt" :explorer="tx.txHash.value ? explorerTx(position.chainId, tx.txHash.value, chains.get(position.chainId)) : null" />
    </div>
  </ModalShell>
</template>

<style scoped>
.body {
  padding: 14px 18px 18px;
  display: flex;
  flex-direction: column;
  gap: 13px;
}
.risk-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  font-size: 11.5px;
  color: var(--fg-muted);
}
.withdraw {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.withdraw .v {
  color: oklch(0.92 0.005 265);
}
.danger-range {
  accent-color: oklch(0.72 0.16 25);
}
.preset-exit {
  flex: 1;
  font-family: var(--font-mono);
  font-size: 11px;
  font-weight: 600;
  padding: 6px;
  border-radius: 8px;
  border: 1px solid var(--border-strong);
  background: transparent;
  color: oklch(0.72 0.01 265);
}
.preset-exit[aria-pressed='true'] {
  border-color: oklch(0.5 0.12 25);
  color: oklch(0.85 0.13 25);
}
.receive {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 10px;
  border: 1px solid oklch(0.28 0.015 265);
  border-radius: 11px;
  padding: 11px 13px;
}
.amounts {
  text-align: right;
  min-width: 0;
}
.out {
  display: block;
  font-size: 21px;
  font-weight: 600;
  overflow-wrap: anywhere;
}
.usd {
  display: block;
  font-size: 11px;
  color: var(--fg-muted);
  margin-top: 2px;
}
.fees {
  color: oklch(0.84 0.13 168) !important;
}
</style>
