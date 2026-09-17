<script setup lang="ts">
export type ChipTone = 'accent' | 'warn' | 'danger' | 'info' | 'muted' | 'ws'

withDefaults(defineProps<{ tone?: ChipTone }>(), { tone: 'muted' })
</script>

<template>
  <span class="chip" :data-tone="tone"><slot /></span>
</template>

<style scoped>
.chip {
  display: inline-block;
  font-family: var(--font-mono);
  font-size: 10px;
  font-weight: 600;
  padding: 4px 8px;
  border-radius: 6px;
  white-space: nowrap;
  letter-spacing: 0.02em;
}
[data-tone='accent'] {
  color: var(--risk-safe-fg);
  background: var(--risk-safe-bg);
}
[data-tone='warn'] {
  color: var(--risk-watch-fg);
  background: var(--risk-watch-bg);
}
[data-tone='danger'] {
  color: oklch(0.78 0.16 25);
  background: var(--risk-danger-bg);
}
[data-tone='info'] {
  color: var(--info-fg);
  background: var(--info-bg);
}
[data-tone='ws'] {
  color: var(--ws-fg);
  background: var(--ws-bg);
}
[data-tone='muted'] {
  color: oklch(0.78 0.01 265);
  background: var(--chip-active);
}
</style>
