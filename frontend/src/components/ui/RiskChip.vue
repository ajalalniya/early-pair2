<script setup lang="ts">
import { computed } from 'vue'
import { band } from '@/lib/risk'
import type { RiskSummary } from '@/types/api'

const props = withDefaults(defineProps<{ risk: RiskSummary | null; showScore?: boolean; size?: 'sm' | 'md' }>(), {
  showScore: true,
  size: 'sm',
})

const b = computed(() => (props.risk ? band(props.risk.score) : null))
const title = computed(() => {
  if (!props.risk) return 'Not scored yet'
  return props.risk.complete ? `Risk score ${props.risk.score} / 100` : `Risk score ${props.risk.score} / 100 — some checks still pending`
})
</script>

<template>
  <span
    class="risk"
    :class="size"
    :style="b ? { color: b.fg, background: b.bg } : undefined"
    :data-band="b?.id ?? 'unscored'"
    :title="title"
  >
    <template v-if="b && risk">{{ showScore ? `${b.label} ${risk.score}` : b.label }}<sup v-if="!risk.complete" aria-hidden="true">*</sup></template>
    <template v-else>UNSCORED</template>
  </span>
</template>

<style scoped>
.risk {
  display: inline-block;
  font-family: var(--font-mono);
  font-weight: 600;
  border-radius: 6px;
  white-space: nowrap;
  color: var(--fg-3);
  background: var(--chip);
}
.sm {
  font-size: 11px;
  padding: 4px 8px;
}
.md {
  font-size: 11px;
  padding: 5px 9px;
}
sup {
  font-size: 9px;
  margin-left: 1px;
}
</style>
