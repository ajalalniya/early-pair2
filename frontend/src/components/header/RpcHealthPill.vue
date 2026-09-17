<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useRpcHealthQuery } from '@/queries'
import { useStreamStore } from '@/stores/stream'
import RpcHealthPanel from './RpcHealthPanel.vue'

const open = ref(false)
const root = ref<HTMLElement | null>(null)
const health = useRpcHealthQuery()
const stream = useStreamStore()

const summary = computed(() => health.data.value?.summary)
const text = computed(() => {
  const s = summary.value
  if (!s) return health.isError.value ? 'RPC health unavailable' : 'RPC —'
  return `${s.healthy}/${s.total} RPCs HEALTHY · p50 ${s.medianP50Ms ?? '—'} ms`
})
const dot = computed(() => {
  const eps = health.data.value?.endpoints ?? []
  if ((stream.status === 'down' && !stream.connecting) || eps.some((e) => e.state === 'UNREACHABLE')) return 'oklch(0.7 0.18 25)'
  if (stream.status === 'polling' || eps.some((e) => e.state === 'DEGRADED')) return 'var(--warn)'
  return 'var(--accent)'
})

function onDocClick(e: MouseEvent) {
  if (root.value && !root.value.contains(e.target as Node)) open.value = false
}
function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape') open.value = false
}
watch(open, (v) => {
  if (v) {
    document.addEventListener('mousedown', onDocClick)
    document.addEventListener('keydown', onKey)
  } else {
    document.removeEventListener('mousedown', onDocClick)
    document.removeEventListener('keydown', onKey)
  }
})
onBeforeUnmount(() => {
  document.removeEventListener('mousedown', onDocClick)
  document.removeEventListener('keydown', onKey)
})
</script>

<template>
  <div ref="root" class="wrap">
    <button class="pill-btn" type="button" :aria-expanded="open" aria-haspopup="dialog" @click="open = !open">
      <span class="dot" :style="{ background: dot }" />
      <span class="text">{{ text }}</span>
      <span class="caret" aria-hidden="true">{{ open ? '▴' : '▾' }}</span>
    </button>
    <RpcHealthPanel v-if="open" :health="health.data.value" :error="health.error.value" />
  </div>
</template>

<style scoped>
.wrap {
  position: relative;
}
.pill-btn {
  display: flex;
  align-items: center;
  gap: 8px;
  font-family: var(--font-mono);
  font-size: 11px;
  color: oklch(0.78 0.01 265);
  background: transparent;
  border: 1px solid var(--chip-2);
  border-radius: 8px;
  padding: 7px 10px;
  white-space: nowrap;
}
.pill-btn:hover {
  border-color: oklch(0.42 0.02 265);
}
.dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  animation: pulseDot 1.8s infinite;
}
.caret {
  color: oklch(0.6 0.01 265);
}
@media (max-width: 519px) {
  .text {
    max-width: 120px;
    overflow: hidden;
    text-overflow: ellipsis;
  }
}
</style>
