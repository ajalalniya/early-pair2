<script setup lang="ts">
import { computed } from 'vue'
import ModalShell from '@/components/ui/ModalShell.vue'
import Chip from '@/components/ui/Chip.vue'
import { addr, money, trimDecimal } from '@/lib/format'
import { EVM_CHAINS, WALLET_OPTIONS, resolveOption, type Badge } from '@/lib/wagmi'
import { useChainsQuery, useFundsQuery } from '@/queries'
import { useWallet } from '@/stores/wallet'

const wallet = useWallet()
const chains = useChainsQuery()
const funds = useFundsQuery()

const title = computed(() =>
  wallet.modal === 'done' ? 'Wallet connected' : wallet.modal === 'pending' ? 'Confirm connection' : 'Connect a wallet',
)

const options = computed(() =>
  WALLET_OPTIONS.map((o) => {
    const { badge } = resolveOption(o, wallet.connectors)
    return { ...o, badge }
  }),
)

const badgeTone = (b: Badge) => (b === 'DETECTED' ? 'accent' : 'muted')

const chainCount = computed(() => {
  const evm = chains.data.value?.filter((c) => c.kind === 'evm').length
  return evm ?? EVM_CHAINS.length
})

function nativeOn(chainId: string): string {
  const fc = funds.data.value?.chains.find((c) => c.chainId === chainId)
  const native = fc?.assets.find((a) => a.isNative)
  return native && !native.unavailable ? `${trimDecimal(native.amount, 4)} ${native.symbol}` : '—'
}

// One native balance per chain the backend serves, so a disabled network is
// not listed with a permanent dash.
const balances = computed(() => [
  ...(chains.data.value ?? [])
    .filter((c) => c.kind === 'evm')
    .map((c) => ({ label: c.name, value: wallet.signedIn ? nativeOn(c.id) : '—' })),
  { label: 'Stables', value: wallet.signedIn ? money(funds.data.value?.stables) : '—' },
])
</script>

<template>
  <ModalShell v-if="wallet.modal" :title="title" @close="wallet.closeModal()">
    <template v-if="wallet.modal === 'choose'">
      <div class="connectors">
        <button
          v-for="o in options"
          :key="o.id"
          type="button"
          class="connector"
          :data-testid="`connector-${o.id}`"
          :disabled="o.badge === 'UNAVAILABLE'"
          @click="wallet.connectWith(o)"
        >
          <span class="logo" :style="{ background: o.tint }">{{ o.tag }}</span>
          <span class="meta">
            <span class="name">{{ o.name }}</span>
            <span class="note">{{ o.badge === 'UNAVAILABLE' ? 'Not configured for this deployment' : o.note }}</span>
          </span>
          <Chip :tone="badgeTone(o.badge)">{{ o.badge === 'UNAVAILABLE' ? 'OFF' : o.badge }}</Chip>
        </button>
      </div>
      <div v-if="wallet.error" class="error" role="alert">{{ wallet.error }}</div>
      <p class="foot">EarlyPair never takes custody. Signing only proves address ownership — trades are approved per transaction.</p>
    </template>

    <div v-else-if="wallet.modal === 'pending'" class="pending">
      <span class="spinner" aria-hidden="true" />
      <span class="approve">Approve in {{ wallet.pendingWallet ?? 'your wallet' }}</span>
      <span class="wait">Waiting for the signature request. Nothing is submitted on-chain.</span>
      <button class="btn" type="button" @click="wallet.cancel()">Cancel</button>
    </div>

    <div v-else class="done">
      <div class="account">
        <span class="avatar" />
        <span class="who">
          <span class="address" data-testid="connected-address">{{ addr(wallet.address) }}</span>
          <span class="via">{{ wallet.connectorName ?? 'Injected wallet' }} · {{ chainCount }} chains</span>
        </span>
        <Chip tone="accent">LIVE</Chip>
      </div>

      <div v-if="!wallet.signedIn" class="notice" data-tone="warn">
        <span>
          {{ wallet.signingIn ? 'Waiting for the sign-in signature…' : wallet.signInError ?? 'Sign in to load balances, portfolio and watchlist.' }}
          <button v-if="!wallet.signingIn" class="inline-link" type="button" @click="wallet.signIn()">Sign in</button>
        </span>
      </div>

      <div class="balances">
        <div v-for="b in balances" :key="b.label" class="bal">
          <span>{{ b.label }}</span>
          <span class="v">{{ b.value }}</span>
        </div>
      </div>

      <div class="actions">
        <button class="btn btn-accent grow" type="button" @click="wallet.closeModal()">Start trading</button>
        <button class="btn btn-danger-outline" type="button" @click="wallet.disconnectWallet()">Disconnect</button>
      </div>
    </div>
  </ModalShell>
</template>

<style scoped>
.connectors {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 14px 18px 6px;
}
.connector {
  text-align: left;
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px 13px;
  border-radius: 12px;
  border: 1px solid oklch(0.28 0.015 265);
  background: var(--surface-tile);
  color: var(--fg);
}
.connector:hover:not(:disabled) {
  border-color: oklch(0.45 0.04 168);
  background: var(--row-selected);
}
.connector:disabled {
  opacity: 0.55;
}
.logo {
  flex: none;
  width: 32px;
  height: 32px;
  border-radius: 9px;
  color: oklch(0.16 0.02 265);
  font-weight: 700;
  font-size: 13px;
  display: flex;
  align-items: center;
  justify-content: center;
  font-family: var(--font-mono);
}
.meta {
  flex: 1;
  min-width: 0;
}
.name {
  display: block;
  font-size: 13.5px;
  font-weight: 600;
}
.note {
  display: block;
  font-size: 11px;
  font-family: var(--font-mono);
  color: var(--fg-muted);
  margin-top: 2px;
}
.error {
  margin: 8px 18px 0;
  font-family: var(--font-mono);
  font-size: 11.5px;
  color: var(--danger-text);
}
.foot {
  margin: 0;
  padding: 12px 18px 16px;
  font-size: 11.5px;
  font-family: var(--font-mono);
  line-height: 1.6;
  color: oklch(0.6 0.01 265);
  text-wrap: pretty;
}
.pending {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 14px;
  padding: 34px 24px 30px;
  text-align: center;
}
.spinner {
  width: 42px;
  height: 42px;
  border-radius: 50%;
  border: 2px solid var(--border-strong);
  border-top-color: var(--accent);
  animation: spinRing 0.9s linear infinite;
}
.approve {
  font-size: 14px;
  font-weight: 600;
}
.wait {
  font-size: 12px;
  font-family: var(--font-mono);
  color: var(--fg-muted);
  line-height: 1.6;
  max-width: 270px;
  text-wrap: pretty;
}
.done {
  padding: 16px 18px 18px;
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.account {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 13px;
  border-radius: 12px;
  border: 1px solid oklch(0.28 0.015 265);
  background: var(--surface-tile);
}
.avatar {
  flex: none;
  width: 34px;
  height: 34px;
  border-radius: 50%;
  background: var(--accent);
}
.who {
  flex: 1;
  min-width: 0;
}
.address {
  display: block;
  font-family: var(--font-mono);
  font-size: 13px;
}
.via {
  display: block;
  font-size: 11px;
  font-family: var(--font-mono);
  color: var(--fg-muted);
  margin-top: 3px;
}
.balances {
  display: flex;
  flex-direction: column;
  gap: 7px;
}
.bal {
  display: flex;
  align-items: center;
  justify-content: space-between;
  font-family: var(--font-mono);
  font-size: 12px;
  color: var(--fg-3);
}
.bal .v {
  color: oklch(0.92 0.005 265);
}
.actions {
  display: flex;
  gap: 8px;
}
.grow {
  flex: 1;
}
.inline-link {
  border: none;
  background: transparent;
  color: var(--accent);
  font-weight: 600;
  padding: 0 0 0 4px;
  font-family: var(--font-ui);
}
</style>
