<script setup lang="ts">
import { onMounted } from 'vue'
import { useQueryClient } from '@tanstack/vue-query'
import AppHeader from '@/components/header/AppHeader.vue'
import StreamStrip from '@/components/header/StreamStrip.vue'
import WalletModal from '@/components/wallet/WalletModal.vue'
import ToastHost from '@/components/ui/ToastHost.vue'
import { useEventStream } from '@/composables/useEventStream'
import { useWallet } from '@/stores/wallet'
import { useSettings } from '@/stores/settings'
import { flushPendingTrades } from '@/lib/tradeLog'

useSettings()
useEventStream()
const wallet = useWallet()
const qc = useQueryClient()

onMounted(() => {
  // Trades confirmed in an earlier session whose report did not get through.
  void flushPendingTrades(qc)
  // Restore a previous wallet connection once the feed has painted.
  const init = () => void wallet.init()
  if (typeof window.requestIdleCallback === 'function') window.requestIdleCallback(init, { timeout: 1500 })
  else setTimeout(init, 300)
})
</script>

<template>
  <div class="app">
    <AppHeader />
    <StreamStrip />
    <RouterView />
    <WalletModal />
    <ToastHost />
  </div>
</template>

<style scoped>
.app {
  min-height: 100vh;
  display: flex;
  flex-direction: column;
}
</style>
