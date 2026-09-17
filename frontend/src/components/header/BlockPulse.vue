<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { useNow } from '@/composables/useNow'
import { chainMeta } from '@/lib/chains'
import { num } from '@/lib/format'
import { useStreamStore } from '@/stores/stream'

const stream = useStreamStore()
const now = useNow()
const route = useRoute()

const chainId = computed(() => {
  const filter = typeof route.query.chain === 'string' ? route.query.chain : 'all'
  if (filter !== 'all' && stream.heads[filter]) return filter
  return stream.lastHeadChain
})
const mark = computed(() => (chainId.value ? stream.heads[chainId.value] : undefined))
const seconds = computed(() => (mark.value ? Math.max(0, Math.floor((now.value - mark.value.at) / 1000)) : null))

const tone = computed(() => {
  if (stream.status === 'down' && !stream.connecting) return 'danger'
  if (seconds.value !== null && chainId.value && seconds.value > 3 * chainMeta(chainId.value).blockTime) return 'warn'
  return 'normal'
})
const label = computed(() => (seconds.value === null ? 'BLOCK —' : `BLOCK +${seconds.value}s`))
const title = computed(() =>
  mark.value && chainId.value
    ? `Last head: ${chainMeta(chainId.value).name} #${num(mark.value.block)} · stream ${stream.status}`
    : `No head received yet · stream ${stream.status}`,
)
</script>

<template>
  <span class="pulse" :data-tone="tone" :title="title">{{ label }}</span>
</template>

<style scoped>
.pulse {
  font-family: var(--font-mono);
  font-size: 11px;
  color: oklch(0.66 0.01 265);
  white-space: nowrap;
  min-width: 82px;
  text-align: right;
}
[data-tone='warn'] {
  color: var(--warn);
}
[data-tone='danger'] {
  color: var(--danger);
}
@media (max-width: 819px) {
  .pulse {
    display: none;
  }
}
</style>
