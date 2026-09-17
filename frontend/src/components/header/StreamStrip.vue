<script setup lang="ts">
import { computed } from 'vue'
import { useEventStream } from '@/composables/useEventStream'
import { useStreamStore } from '@/stores/stream'

const stream = useStreamStore()
const handle = useEventStream()

const visible = computed(() => stream.status !== 'live' && !(stream.connecting && stream.lastSeq === null && stream.status === 'down'))
const text = computed(() =>
  stream.status === 'polling' ? 'reconnecting — showing polled data' : 'stream down — the API is unreachable, retrying',
)
</script>

<template>
  <div v-if="visible" class="strip" :data-status="stream.status" role="status" aria-live="polite">
    <span class="dot" />
    <span>{{ text }}</span>
    <button type="button" @click="handle.reconnectNow()">Retry now</button>
  </div>
</template>

<style scoped>
.strip {
  position: sticky;
  top: var(--header-h);
  z-index: 25;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 10px;
  padding: 6px 16px;
  font-family: var(--font-mono);
  font-size: 11.5px;
  color: oklch(0.2 0.03 85);
  background: var(--warn);
}
.strip[data-status='down'] {
  background: var(--danger);
  color: oklch(0.18 0.03 25);
}
.dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: currentColor;
  animation: pulseDot 1.2s infinite;
}
button {
  border: 1px solid currentColor;
  background: transparent;
  color: inherit;
  border-radius: 6px;
  padding: 2px 8px;
  font-size: 11px;
  font-weight: 600;
}
@media (max-width: 819px) {
  .strip {
    top: 0;
    position: relative;
  }
}
</style>
