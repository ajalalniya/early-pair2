<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { describeRange, formatBound, isSet, parseBound, type Range, type RangeKind } from '@/lib/columnFilters'

export interface RangeField {
  /** Row label inside the popover; omit for a single-range filter. */
  label?: string
  kind: RangeKind
  value: Range
  presets: { label: string; range: Range }[]
}

const props = defineProps<{
  /** Chip text, e.g. "LIQUIDITY". */
  name: string
  fields: RangeField[]
  testid: string
}>()
const emit = defineEmits<{ apply: [ranges: Range[]] }>()

const openState = ref(false)
const root = ref<HTMLElement | null>(null)
const firstInput = ref<HTMLInputElement[] | null>(null)

// Text as typed, per field: [min, max].
const drafts = ref<string[][]>([])
function loadDrafts() {
  drafts.value = props.fields.map((f) => [
    f.value.min === null ? '' : formatBound(f.value.min, f.kind),
    f.value.max === null ? '' : formatBound(f.value.max, f.kind),
  ])
}

const active = computed(() => props.fields.some((f) => isSet(f.value)))
const summary = computed(() =>
  props.fields
    .filter((f) => isSet(f.value))
    .map((f) => (f.label ? `${f.label} ` : '') + describeRange(f.value, f.kind))
    .join(' · '),
)

const parsed = computed(() =>
  props.fields.map((f, i) => ({
    min: parseBound(drafts.value[i]?.[0] ?? '', f.kind),
    max: parseBound(drafts.value[i]?.[1] ?? '', f.kind),
  })),
)
const invalid = computed(() =>
  parsed.value.map((r) => ({
    min: r.min !== null && Number.isNaN(r.min),
    max: r.max !== null && Number.isNaN(r.max),
    order: r.min !== null && r.max !== null && !Number.isNaN(r.min) && !Number.isNaN(r.max) && r.min > r.max,
  })),
)
const canApply = computed(() => invalid.value.every((v) => !v.min && !v.max && !v.order))

function toggle() {
  if (openState.value) return close()
  loadDrafts()
  openState.value = true
  void nextTick(() => firstInput.value?.[0]?.focus())
}

function close() {
  openState.value = false
}

function apply() {
  if (!canApply.value) return
  emit('apply', parsed.value.map((r) => ({ min: r.min, max: r.max })))
  close()
}

function preset(i: number, range: Range) {
  const f = props.fields[i]!
  drafts.value[i] = [range.min === null ? '' : formatBound(range.min, f.kind), range.max === null ? '' : formatBound(range.max, f.kind)]
}

function clearAll() {
  emit('apply', props.fields.map(() => ({ min: null, max: null })))
  close()
}

function onDocDown(e: MouseEvent) {
  if (openState.value && root.value && !root.value.contains(e.target as Node)) close()
}
watch(openState, (v) => {
  if (v) document.addEventListener('mousedown', onDocDown)
  else document.removeEventListener('mousedown', onDocDown)
})
onBeforeUnmount(() => document.removeEventListener('mousedown', onDocDown))

const placeholder = (kind: RangeKind, side: 'min' | 'max') =>
  ({ usd: side === 'min' ? '20k' : '1m', count: side === 'min' ? '10' : '500', pct: side === 'min' ? '-20' : '+100', minutes: side === 'min' ? '0' : '30m' })[kind]
</script>

<template>
  <div ref="root" class="range-filter" @keydown.esc.stop="close">
    <button
      type="button"
      class="pill"
      :aria-pressed="active"
      :aria-expanded="openState"
      :data-testid="`filter-${testid}`"
      @click="toggle"
    >
      <span>{{ name }}</span>
      <span v-if="active" class="val mono">{{ summary }}</span>
      <span v-else class="caret" aria-hidden="true">▾</span>
    </button>
    <button
      v-if="active"
      type="button"
      class="x"
      :aria-label="`Clear ${name} filter`"
      :data-testid="`filter-${testid}-clear`"
      @click="clearAll"
    >
      ×
    </button>

    <form v-if="openState" class="pop card" :data-testid="`filter-${testid}-panel`" @submit.prevent="apply">
      <div v-for="(f, i) in fields" :key="i" class="field">
        <div v-if="f.label" class="f-label mono">{{ f.label }}</div>
        <div class="inputs">
          <input
            ref="firstInput"
            v-model="drafts[i]![0]"
            class="mono"
            :placeholder="`min ${placeholder(f.kind, 'min')}`"
            :aria-label="`${f.label ?? name} minimum`"
            :aria-invalid="invalid[i]!.min || invalid[i]!.order"
            :data-testid="`filter-${testid}-min${i || ''}`"
          />
          <span class="dash">–</span>
          <input
            v-model="drafts[i]![1]"
            class="mono"
            :placeholder="`max ${placeholder(f.kind, 'max')}`"
            :aria-label="`${f.label ?? name} maximum`"
            :aria-invalid="invalid[i]!.max || invalid[i]!.order"
            :data-testid="`filter-${testid}-max${i || ''}`"
          />
        </div>
        <div class="presets">
          <button v-for="p in f.presets" :key="p.label" type="button" class="preset" @click="preset(i, p.range)">{{ p.label }}</button>
        </div>
        <div v-if="invalid[i]!.order" class="err mono">Minimum is above maximum.</div>
        <div v-else-if="invalid[i]!.min || invalid[i]!.max" class="err mono">
          {{ f.kind === 'minutes' ? 'Use a number of minutes, or 30m, 2h, 1d.' : f.kind === 'pct' ? 'Use a percentage like -20 or 50.' : 'Use a number like 20000, 20k or 1.5m.' }}
        </div>
      </div>
      <div class="foot">
        <button type="button" class="btn btn-sm" @click="clearAll">Clear</button>
        <button type="submit" class="btn btn-sm btn-accent" :disabled="!canApply" :data-testid="`filter-${testid}-apply`">Apply</button>
      </div>
    </form>
  </div>
</template>

<style scoped>
.range-filter {
  position: relative;
  display: inline-flex;
  align-items: center;
}
.val {
  font-size: 11.5px;
  color: var(--accent);
}
.caret {
  font-size: 9px;
  color: var(--fg-muted);
}
.x {
  margin-left: -6px;
  width: 20px;
  height: 20px;
  display: grid;
  place-items: center;
  padding: 0;
  border: none;
  border-radius: 50%;
  background: transparent;
  color: var(--fg-muted);
  font-size: 14px;
  line-height: 1;
  cursor: pointer;
}
.x:hover {
  color: var(--fg);
  background: var(--chip);
}
.pop {
  position: absolute;
  top: calc(100% + 6px);
  left: 0;
  z-index: 30;
  width: 250px;
  padding: 12px;
  display: flex;
  flex-direction: column;
  gap: 12px;
  box-shadow: 0 12px 32px oklch(0 0 0 / 0.45);
}
.field {
  display: flex;
  flex-direction: column;
  gap: 7px;
}
.f-label {
  font-size: 10.5px;
  letter-spacing: 0.06em;
  color: var(--fg-muted);
}
.inputs {
  display: flex;
  align-items: center;
  gap: 6px;
}
.inputs input {
  flex: 1;
  min-width: 0;
  padding: 6px 8px;
  border: 1px solid var(--border);
  border-radius: 7px;
  background: transparent;
  color: var(--fg);
  font-size: 12px;
}
.inputs input:focus {
  outline: none;
  border-color: var(--border-active);
}
.inputs input[aria-invalid='true'] {
  border-color: var(--danger);
}
.dash {
  color: var(--fg-faint);
}
.presets {
  display: flex;
  flex-wrap: wrap;
  gap: 5px;
}
.presets .preset {
  font-size: 11px;
  padding: 3px 7px;
  cursor: pointer;
}
.err {
  font-size: 10.5px;
  color: var(--danger-text);
}
.foot {
  display: flex;
  justify-content: flex-end;
  gap: 6px;
}
@media (max-width: 819px) {
  .pop {
    position: fixed;
    left: 16px;
    right: 16px;
    top: auto;
    width: auto;
  }
}
</style>
