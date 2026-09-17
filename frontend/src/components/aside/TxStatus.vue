<script setup lang="ts">
import { computed } from 'vue'
import { hash as fmtHash } from '@/lib/format'
import type { useTxExecutor } from '@/composables/useTxExecutor'

const props = defineProps<{ tx: ReturnType<typeof useTxExecutor>; receipt: string; explorer: string | null }>()

const phase = computed(() => props.tx.phase.value)
</script>

<template>
  <div v-if="phase === 'done'" class="receipt mono" role="status" data-testid="tx-receipt">
    <a v-if="explorer" :href="explorer" target="_blank" rel="noopener noreferrer">{{ receipt }}</a>
    <span v-else>{{ receipt }}</span>
  </div>
  <div v-else-if="phase === 'error'" class="notice" data-tone="danger" role="alert" data-testid="tx-error">{{ tx.error.value }}</div>
  <div v-else-if="phase !== 'idle' && tx.stepLabel.value" class="step mono" role="status">
    {{ tx.stepLabel.value }}<template v-if="tx.txHash.value"> · {{ fmtHash(tx.txHash.value) }}</template>
  </div>
</template>

<style scoped>
.receipt {
  font-size: 11.5px;
  text-align: center;
  color: oklch(0.84 0.11 168);
}
.receipt a {
  color: inherit;
  text-decoration: underline;
  text-decoration-color: oklch(0.84 0.11 168 / 0.4);
  text-underline-offset: 3px;
}
.step {
  font-size: 11px;
  text-align: center;
  color: var(--fg-muted);
}
</style>
