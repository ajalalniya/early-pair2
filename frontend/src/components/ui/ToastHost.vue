<script setup lang="ts">
import { useToasts } from '@/stores/toasts'

const toasts = useToasts()
</script>

<template>
  <div class="host" role="status" aria-live="polite">
    <TransitionGroup name="toast">
      <div v-for="t in toasts.items" :key="t.id" class="toast" :data-tone="t.tone">
        <span>{{ t.message }}</span>
        <button type="button" aria-label="Dismiss" @click="toasts.dismiss(t.id)">✕</button>
      </div>
    </TransitionGroup>
  </div>
</template>

<style scoped>
.host {
  position: fixed;
  right: 16px;
  bottom: 16px;
  z-index: 120;
  display: flex;
  flex-direction: column;
  gap: 8px;
  width: min(360px, calc(100vw - 32px));
  pointer-events: none;
}
.toast {
  pointer-events: auto;
  display: flex;
  align-items: flex-start;
  gap: 10px;
  padding: 11px 13px;
  border-radius: 11px;
  border: 1px solid var(--border-strong);
  background: var(--surface-raised);
  font-family: var(--font-mono);
  font-size: 11.5px;
  line-height: 1.5;
  color: var(--fg-2);
}
.toast span {
  flex: 1;
}
.toast[data-tone='warn'] {
  border-color: oklch(0.4 0.07 85);
}
.toast[data-tone='danger'] {
  border-color: var(--danger-border);
}
.toast[data-tone='accent'] {
  border-color: oklch(0.4 0.07 168);
}
.toast button {
  border: none;
  background: transparent;
  color: var(--fg-muted);
  padding: 0 2px;
}
.toast-enter-active,
.toast-leave-active {
  transition:
    opacity 0.2s,
    transform 0.2s;
}
.toast-enter-from,
.toast-leave-to {
  opacity: 0;
  transform: translateY(6px);
}
</style>
