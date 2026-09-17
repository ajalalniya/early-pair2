<script setup lang="ts">
import { computed } from 'vue'
import { band, checkRows, verdict } from '@/lib/risk'
import { useSettings } from '@/stores/settings'
import type { Pool } from '@/types/api'

const props = defineProps<{ pool: Pool }>()
const settings = useSettings()

const b = computed(() => (props.pool.risk ? band(props.pool.risk.score) : null))
const rows = computed(() => checkRows(props.pool))
const text = computed(() => verdict(props.pool, settings.riskGateThreshold))
</script>

<template>
  <section class="card fraud" aria-labelledby="fraud-title" data-testid="fraud-card">
    <div class="top">
      <span id="fraud-title" class="caps">FRAUD DETECTION</span>
      <span class="score mono" :style="{ color: b?.fg ?? 'var(--fg-3)' }">{{ pool.risk ? pool.risk.score : '—' }} / 100</span>
    </div>
    <div class="meter" role="meter" :aria-valuenow="pool.risk?.score ?? 0" aria-valuemin="0" aria-valuemax="100" aria-label="Risk score">
      <div :style="{ width: (pool.risk?.score ?? 0) + '%', background: b?.fg ?? 'transparent' }" />
    </div>
    <p class="verdict">{{ text }}</p>
    <p v-if="pool.risk && !pool.risk.complete" class="pending mono">Some checks are still running; the score may change.</p>
    <ul class="checks">
      <li v-for="c in rows" :key="c.label" :data-state="c.state">
        <span class="dot" :aria-label="c.state === 'pass' ? 'PASS' : c.state === 'fail' ? 'FAIL' : 'NOT CHECKED'" />
        <span class="label">{{ c.label }}</span>
        <span class="value mono">{{ c.value }}</span>
      </li>
    </ul>
  </section>
</template>

<style scoped>
.fraud {
  padding: 16px;
}
.top {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.top .caps {
  letter-spacing: 0.07em;
}
.score {
  font-size: 13px;
  font-weight: 600;
}
.meter {
  height: 5px;
  border-radius: 999px;
  background: var(--chip-2);
  margin-top: 10px;
  overflow: hidden;
}
.meter > div {
  height: 100%;
  border-radius: 999px;
  transition: width 0.3s ease;
}
.verdict {
  font-size: 12.5px;
  line-height: 1.5;
  color: oklch(0.78 0.01 265);
  margin: 11px 0 0;
  text-wrap: pretty;
}
.pending {
  margin: 6px 0 0;
  font-size: 10.5px;
  color: var(--warn-text);
}
.checks {
  list-style: none;
  padding: 0;
  margin: 13px 0 0;
  display: flex;
  flex-direction: column;
  gap: 1px;
}
li {
  display: flex;
  align-items: center;
  gap: 9px;
  padding: 8px 0;
  border-top: 1px solid oklch(0.235 0.012 265);
}
.dot {
  flex: none;
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: oklch(0.5 0.01 265);
}
[data-state='pass'] .dot {
  background: oklch(0.8 0.14 168);
}
[data-state='fail'] .dot {
  background: var(--danger);
}
.label {
  flex: 1;
  font-size: 12.5px;
  color: oklch(0.74 0.01 265);
}
.value {
  font-size: 11.5px;
  text-align: right;
  color: oklch(0.88 0.01 265);
}
[data-state='fail'] .value {
  color: oklch(0.78 0.14 25);
}
[data-state='unknown'] .value {
  color: var(--fg-muted);
}
</style>
