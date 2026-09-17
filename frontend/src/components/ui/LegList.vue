<script setup lang="ts">
import type { QuoteLeg } from '@/types/api'

defineProps<{ legs: QuoteLeg[] }>()
</script>

<template>
  <ol class="legs" data-testid="legs">
    <li v-for="l in legs" :key="l.step">
      <span class="step mono">{{ l.step }}</span>
      <span class="leg-label">{{ l.label }}</span>
      <span class="leg-value mono">{{ l.value }}</span>
    </li>
  </ol>
</template>

<style scoped>
.legs {
  list-style: none;
  margin: 0;
  padding: 0;
  border: 1px solid var(--border-soft);
  border-radius: 11px;
  background: var(--surface-inset);
  overflow: hidden;
}
li {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 12px;
  border-bottom: 1px solid oklch(0.235 0.012 265);
}
li:last-child {
  border-bottom: none;
}
.step {
  flex: none;
  width: 20px;
  height: 20px;
  border-radius: 6px;
  background: var(--chip-active);
  color: oklch(0.8 0.01 265);
  font-size: 10px;
  font-weight: 700;
  display: flex;
  align-items: center;
  justify-content: center;
}
.leg-label {
  flex: 1;
  min-width: 0;
  font-size: 12.5px;
}
.leg-value {
  font-size: 11.5px;
  color: var(--fg-2);
  white-space: nowrap;
}
</style>
