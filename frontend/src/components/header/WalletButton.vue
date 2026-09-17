<script setup lang="ts">
import { addr } from '@/lib/format'
import { useWallet } from '@/stores/wallet'

const wallet = useWallet()
</script>

<template>
  <button
    class="wallet"
    type="button"
    :data-connected="wallet.connected"
    data-testid="wallet-button"
    @click="wallet.openModal()"
  >
    {{ wallet.connected ? addr(wallet.address) : 'Connect Wallet' }}
  </button>
</template>

<style scoped>
.wallet {
  font-size: 13px;
  font-weight: 600;
  border: none;
  border-radius: 9px;
  padding: 9px 16px;
  background: var(--accent);
  color: var(--on-accent);
  white-space: nowrap;
}
.wallet[data-connected='true'] {
  background: var(--chip-active);
  color: var(--fg);
  font-family: var(--font-mono);
  font-weight: 500;
}
.wallet:hover {
  opacity: 0.88;
}
</style>
