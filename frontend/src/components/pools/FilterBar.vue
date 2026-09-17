<script setup lang="ts">
import { computed } from 'vue'
import RangeFilter, { type RangeField } from './RangeFilter.vue'
import { CHAIN_META } from '@/lib/chains'
import { activeCount, COLUMN_KIND, emptyFilters, type ColumnFilters, type ColumnKey, type Range } from '@/lib/columnFilters'
import { ALL_DEXES, dexOptions, dexSlug } from '@/lib/dexFilter'
import type { RiskFilter } from '@/lib/risk'
import type { ChainInfo } from '@/types/api'

const props = defineProps<{ chain: string; risk: RiskFilter; dex: string; chains: ChainInfo[] | undefined; columns: ColumnFilters }>()
const emit = defineEmits<{ chain: [id: string]; risk: [id: RiskFilter]; dex: [slug: string]; columns: [filters: ColumnFilters] }>()

// One chip per DEX the backend watches, narrowed to the selected chain. Hidden
// entirely when there is nothing to choose between.
const dexChips = computed(() => {
  const options = dexOptions(props.chains, props.chain)
  if (options.length < 2) return []
  return [{ slug: ALL_DEXES, name: 'All' }, ...options.map((d) => ({ slug: dexSlug(d), name: d }))]
})

const atLeast = (label: string, min: number) => ({ label, range: { min, max: null } })
const atMost = (label: string, max: number) => ({ label, range: { min: null, max } })

// One chip per table column. BUYERS / SELLERS holds both ranges, as the column does.
const columnChips = computed<{ name: string; testid: string; keys: ColumnKey[]; fields: RangeField[] }[]>(() => {
  const field = (k: ColumnKey, presets: RangeField['presets'], label?: string): RangeField => ({ label, kind: COLUMN_KIND[k], value: props.columns[k], presets })
  return [
    { name: 'AGE', testid: 'age', keys: ['age'], fields: [field('age', [atMost('≤ 5m', 5), atMost('≤ 30m', 30), atMost('≤ 1h', 60), atMost('≤ 24h', 1440)])] },
    { name: 'LIQUIDITY', testid: 'liq', keys: ['liq'], fields: [field('liq', [atLeast('≥ $10K', 1e4), atLeast('≥ $20K', 2e4), atLeast('≥ $50K', 5e4), atLeast('≥ $100K', 1e5)])] },
    { name: 'VOL 1H', testid: 'vol', keys: ['vol'], fields: [field('vol', [atLeast('≥ $1K', 1e3), atLeast('≥ $10K', 1e4), atLeast('≥ $50K', 5e4), atLeast('≥ $100K', 1e5)])] },
    {
      name: 'BUYERS / SELLERS',
      testid: 'traders',
      keys: ['buy', 'sell'],
      fields: [
        field('buy', [atLeast('≥ 10', 10), atLeast('≥ 50', 50), atLeast('≥ 100', 100)], 'BUYERS'),
        field('sell', [atLeast('≥ 5', 5), atLeast('≥ 20', 20), atLeast('≥ 50', 50)], 'SELLERS'),
      ],
    },
    { name: 'PRICE 1H', testid: 'chg', keys: ['chg'], fields: [field('chg', [atLeast('≥ +10%', 10), atLeast('≥ +50%', 50), atLeast('≥ +100%', 100), atMost('≤ −20%', -20)])] },
  ]
})

function applyColumn(keys: ColumnKey[], ranges: Range[]) {
  const next = { ...props.columns }
  keys.forEach((k, i) => (next[k] = ranges[i] ?? { min: null, max: null }))
  emit('columns', next)
}

const activeColumns = computed(() => activeCount(props.columns))

const chainChips = computed(() => {
  const served = new Map((props.chains ?? []).map((c) => [c.id, c]))
  const list = CHAIN_META.map((m) => {
    const info = served.get(m.id)
    return {
      id: m.id,
      name: info?.name ?? m.name,
      dot: m.dot,
      // Say plainly when a chain is not indexed, instead of an empty feed that reads as "no new pools".
      note: !props.chains ? '' : !info ? 'Not configured on this backend' : !info.ingesting ? 'Configured, not ingesting yet' : '',
    }
  })
  // Chains the backend serves that the design did not anticipate still get a chip.
  for (const c of props.chains ?? []) {
    if (!CHAIN_META.some((m) => m.id === c.id)) list.push({ id: c.id, name: c.name, dot: 'var(--chain-unknown)', note: c.ingesting ? '' : 'Configured, not ingesting yet' })
  }
  return [{ id: 'all', name: 'All', dot: 'var(--chain-unknown)', note: '' }, ...list]
})

const riskChips: { id: RiskFilter; name: string }[] = [
  { id: 'all', name: 'All' },
  { id: 'safe', name: 'Safe' },
  { id: 'watch', name: 'Watch' },
  { id: 'danger', name: 'Danger' },
]
</script>

<template>
  <div class="bar">
    <div class="group" role="group" aria-label="Chain filter">
      <span class="label">CHAIN</span>
      <button
        v-for="c in chainChips"
        :key="c.id"
        type="button"
        class="pill"
        :class="{ idle: !!c.note }"
        :aria-pressed="chain === c.id"
        :title="c.note || undefined"
        @click="emit('chain', c.id)"
      >
        <span class="dot" :style="{ background: c.dot }" />
        {{ c.name }}
      </button>
    </div>
    <div class="divider" />
    <div class="group" role="group" aria-label="Risk filter">
      <span class="label">RISK</span>
      <button v-for="r in riskChips" :key="r.id" type="button" class="pill" :aria-pressed="risk === r.id" @click="emit('risk', r.id)">
        {{ r.name }}
      </button>
    </div>
    <template v-if="dexChips.length">
      <div class="divider" />
      <div class="group" role="group" aria-label="DEX filter">
        <span class="label">DEX</span>
        <button
          v-for="d in dexChips"
          :key="d.slug"
          type="button"
          class="pill"
          :data-testid="`dex-${d.slug}`"
          :aria-pressed="dex === d.slug"
          @click="emit('dex', d.slug)"
        >
          {{ d.name }}
        </button>
      </div>
    </template>
    <div class="break" />
    <div class="group" role="group" aria-label="Column filters">
      <span class="label">FILTER</span>
      <RangeFilter
        v-for="c in columnChips"
        :key="c.testid"
        :name="c.name"
        :testid="c.testid"
        :fields="c.fields"
        @apply="(ranges) => applyColumn(c.keys, ranges)"
      />
      <button v-if="activeColumns > 1" type="button" class="link" data-testid="filter-clear-all" @click="emit('columns', emptyFilters())">
        Clear all
      </button>
    </div>
  </div>
</template>

<style scoped>
.bar {
  display: flex;
  gap: 8px;
  align-items: center;
  flex-wrap: wrap;
  border: 1px solid var(--chip-2);
  border-radius: 12px;
  padding: 10px 12px;
  background: var(--surface);
}
.group {
  display: contents;
}
.label {
  font-size: 11px;
  font-family: var(--font-mono);
  color: oklch(0.66 0.01 265);
  letter-spacing: 0.06em;
}
.dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
}
.idle:not([aria-pressed='true']) {
  color: var(--fg-muted);
}
.idle .dot {
  opacity: 0.45;
}
.break {
  flex-basis: 100%;
  height: 0;
}
.link {
  border: none;
  background: transparent;
  color: var(--accent);
  font-size: 12px;
  padding: 0 4px;
  cursor: pointer;
}
.divider {
  width: 1px;
  height: 22px;
  background: oklch(0.28 0.015 265);
  margin: 0 4px;
}
@media (max-width: 819px) {
  .divider {
    flex-basis: 100%;
    height: 0;
    margin: 0;
  }
}
</style>
