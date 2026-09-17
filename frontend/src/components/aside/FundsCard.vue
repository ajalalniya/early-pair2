<script setup lang="ts">
import { computed } from 'vue'
import ChainTag from '@/components/ui/ChainTag.vue'
import { errorMessage } from '@/lib/api'
import { CHAIN_META, chainMeta } from '@/lib/chains'
import { DASH, money, trimDecimal } from '@/lib/format'
import { useChainsQuery, useFundsQuery } from '@/queries'
import { useWallet } from '@/stores/wallet'
import type { FundsAsset } from '@/types/api'

const wallet = useWallet()
const funds = useFundsQuery()
const chains = useChainsQuery()

const live = computed(() => wallet.signedIn && !!funds.data.value)

/** A balance read as exactly zero. One that could not be read is unknown, not zero, and stays listed. */
const isZero = (a: FundsAsset) => !a.unavailable && !/[1-9]/.test(a.amount)

const cards = computed(() => {
  if (live.value) {
    // Only what the wallet holds: zero balances are left out, and so is any
    // network with nothing left to show.
    return funds
      .data.value!.chains.map((c) => ({
        chainId: c.chainId,
        name: c.name,
        total: money(c.totalUsd),
        assets: c.assets
          .filter((a) => !isZero(a))
          .map((a) => ({
            sym: a.symbol,
            stable: a.isStable,
            amt: a.unavailable ? DASH : trimDecimal(a.amount, a.isStable ? 2 : 4),
            usd: money(a.usd),
          })),
      }))
      .filter((c) => c.assets.length > 0)
  }
  const ids = chains.data.value?.map((c) => c.id) ?? CHAIN_META.map((c) => c.id)
  return ids.map((id) => ({
    chainId: id,
    name: chainMeta(id, chains.data.value?.find((c) => c.id === id)).name,
    total: DASH,
    assets: [] as { sym: string; stable: boolean; amt: string; usd: string }[],
  }))
})

const subtitle = computed(() => {
  if (!wallet.connected) return 'Connect a wallet to see balances'
  if (!wallet.signedIn) return 'Sign in to load balances'
  if (funds.isError.value) return errorMessage(funds.error.value)
  if (!funds.data.value) return 'Reading balances from chain…'
  const n = cards.value.length
  return n === 0 ? 'Native + stables, read from every chain' : `Native + stables on ${n} ${n === 1 ? 'network' : 'networks'}`
})
</script>

<template>
  <section class="card funds" aria-labelledby="funds-title">
    <div class="top">
      <div>
        <div id="funds-title" class="caps">AVAILABLE FUNDS</div>
        <div class="total mono">{{ live ? money(funds.data.value?.totalUsd) : DASH }}</div>
        <div class="sub">{{ subtitle }}</div>
        <div class="stables mono">Stables {{ live ? money(funds.data.value?.stables) : DASH }}</div>
      </div>
      <button v-if="!wallet.connected" class="btn btn-accent" type="button" @click="wallet.openModal()">Connect</button>
      <button v-else-if="!wallet.signedIn" class="btn" type="button" :disabled="wallet.signingIn" @click="wallet.signIn()">
        {{ wallet.signingIn ? 'Signing…' : 'Sign in' }}
      </button>
    </div>
    <div v-if="live && cards.length === 0" class="asset mono none" data-testid="funds-empty"><span class="sym">No balances on any network</span></div>
    <div v-else class="grid" :class="{ idle: !live }">
      <div v-for="c in cards" :key="c.chainId" class="mini" :data-testid="`funds-${c.chainId}`">
        <div class="mini-head">
          <ChainTag :chain-id="c.chainId" size="wide" />
          <span class="name">{{ c.name }}</span>
          <span class="mono chain-total">{{ c.total }}</span>
        </div>
        <div v-if="c.assets.length" class="assets">
          <div v-for="a in c.assets" :key="a.sym" class="asset mono">
            <span class="sym" :class="{ stable: a.stable }">{{ a.sym }}</span>
            <span class="amt">{{ a.amt }}</span>
            <span class="usd">{{ a.usd }}</span>
          </div>
        </div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.funds {
  padding: 14px 16px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.top {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 12px;
}
.total {
  font-size: 26px;
  font-weight: 600;
  margin-top: 5px;
}
.sub {
  font-size: 11px;
  color: var(--fg-muted);
  margin-top: 3px;
}
.stables {
  font-size: 11px;
  color: var(--fg-3);
  margin-top: 6px;
}
.grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(184px, 1fr));
  gap: 8px;
}
/* Without balances there is nothing but a chain name per card; pack them tighter. */
.grid.idle {
  grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
  gap: 6px;
}
.grid.idle .mini {
  padding: 7px 9px;
}
.mini {
  border: 1px solid var(--border-soft);
  border-radius: 11px;
  padding: 9px 11px;
  display: flex;
  flex-direction: column;
  gap: 7px;
  min-width: 0;
}
.mini-head {
  display: flex;
  align-items: center;
  gap: 8px;
}
.name {
  flex: 1;
  font-size: 12px;
  font-weight: 600;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.chain-total {
  font-size: 11.5px;
  color: oklch(0.8 0.01 265);
}
.assets {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.asset {
  display: flex;
  align-items: baseline;
  gap: 8px;
  font-size: 11.5px;
}
.sym {
  flex: none;
  width: 42px;
  color: oklch(0.94 0.005 265);
}
.sym.stable {
  color: var(--fg-3);
}
.amt {
  flex: 1;
  text-align: right;
  color: oklch(0.92 0.005 265);
  overflow: hidden;
  text-overflow: ellipsis;
}
.usd {
  flex: none;
  width: 62px;
  text-align: right;
  color: var(--fg-muted);
}
.none .sym {
  width: auto;
  color: var(--fg-muted);
}
</style>
