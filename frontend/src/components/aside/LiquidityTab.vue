<script setup lang="ts">
import { computed, ref, toRef } from 'vue'
import Chip from '@/components/ui/Chip.vue'
import RangeBand from './RangeBand.vue'
import { useTradeContext } from '@/composables/useTradeContext'
import { ApiError } from '@/lib/api'
import { poolKey } from '@/lib/chains'
import { DASH, bps, money, price as fmtPrice } from '@/lib/format'
import { FEE_TIERS_BPS, RANGE_PRESETS, axisView, computeRange, step, type PresetId, type RangeView } from '@/lib/range'
import { usePositionsQuery } from '@/queries'
import type { Pool } from '@/types/api'

const props = defineProps<{ pool: Pool }>()
const pool = toRef(props, 'pool')
const ctx = useTradeContext(pool)
const { wallet } = ctx

const mode = ref<'add' | 'remove'>('add')
const model = computed(() => props.pool.model)

// ------------------------------------------------------------ position
const positions = usePositionsQuery()
const position = computed(() =>
  positions.data.value?.find((p) => poolKey(p.chainId, p.pool) === poolKey(props.pool.chainId, props.pool.address)),
)
const positionLabel = computed(() => {
  if (!wallet.connected) return 'No wallet connected'
  if (!wallet.signedIn) return 'Sign in to load'
  if (positions.error.value instanceof ApiError && positions.error.value.notImplemented) return 'Not tracked yet'
  if (positions.isLoading.value) return 'Loading…'
  if (!position.value) return 'No position'
  const share = position.value.sharePct
  return `${money(position.value.valueUsd)}${share !== null ? ` · ${share.toFixed(2)}% of pool` : ''}`
})

// ------------------------------------------------------------ range (clmm)
const feeTier = ref<number>(FEE_TIERS_BPS.includes(props.pool.feeTierBps as (typeof FEE_TIERS_BPS)[number]) ? props.pool.feeTierBps! : 30)
const preset = ref<PresetId>('mid')
const custom = ref(false)
const minText = ref<string | null>(null)
const maxText = ref<string | null>(null)
const frozen = ref<RangeView | null>(null)

const price = computed(() => props.pool.price)
const range = computed(() =>
  price.value ? computeRange({ price: price.value, preset: preset.value, minText: minText.value, maxText: maxText.value }) : null,
)
const view = computed(() => (frozen.value ?? (price.value && range.value ? axisView(price.value, range.value) : { lo: 0, span: 1 })))

function selectPreset(id: PresetId) {
  preset.value = id
  custom.value = false
  minText.value = null
  maxText.value = null
}
function setMin(text: string) {
  minText.value = text
  custom.value = true
}
function setMax(text: string) {
  maxText.value = text
  custom.value = true
}
function stepBound(which: 'min' | 'max', dir: 1 | -1) {
  if (!price.value || !range.value) return
  if (which === 'min') setMin(step(range.value.minText, dir, price.value))
  else setMax(step(range.value.maxText, dir, price.value))
}

const presetChips = computed(() => [
  ...RANGE_PRESETS.map((p) => ({ id: p.id as PresetId | 'custom', name: p.name })),
  ...(custom.value ? [{ id: 'custom' as const, name: 'Custom' }] : []),
])
const activePreset = computed(() => (custom.value ? 'custom' : preset.value))

const badge = computed(() =>
  model.value === 'clmm'
    ? { text: 'CONCENTRATED', tone: 'info' as const }
    : model.value === 'stable'
      ? { text: 'STABLESWAP', tone: 'accent' as const }
      : { text: 'FULL RANGE · 50 / 50', tone: 'accent' as const },
)
const note = computed(() =>
  model.value === 'clmm'
    ? 'Liquidity is only active while price stays inside your range — narrower earns more fees but goes idle sooner.'
    : model.value === 'stable'
      ? "Deposits enter at the pool's current ratio across the whole curve; an imbalance fee applies if you deposit one-sided."
      : 'Deposits are split 50 / 50 and spread across the entire price curve — no range to manage.',
)

const warning = computed(() => {
  const r = range.value
  if (!r) return null
  if (!r.valid) return 'Min price must be below max price.'
  if (!r.inRange && r.status !== 'Always in range') return 'Price is outside your range — this position earns no fees until it comes back.'
  return null
})

// ------------------------------------------------------------ amounts & action
const amountA = ref('')
const amountB = ref('')
const removePct = ref(100)

const invalidRange = computed(() => mode.value === 'add' && model.value === 'clmm' && !!range.value && !range.value.valid)

const buttonLabel = computed(() => {
  if (invalidRange.value) return 'Fix price range'
  if (!wallet.connected) return 'Connect wallet'
  return mode.value === 'add' ? 'Add liquidity' : 'Remove liquidity'
})

// Removing liquidity is never risk-gated. Adding is not either: the gate covers buys and zaps.
const unavailableReason = computed(() => {
  if (!wallet.connected) return null
  if (mode.value === 'remove' && !position.value) return 'There is no position in this pool to remove.'
  return 'The backend does not build liquidity transactions yet, so this cannot be signed.'
})

function onAction() {
  if (!wallet.connected) wallet.openModal()
}
</script>

<template>
  <div class="tab-body">
    <div class="segmented" role="group" aria-label="Liquidity action">
      <button type="button" :aria-pressed="mode === 'add'" @click="mode = 'add'">Add</button>
      <button type="button" :aria-pressed="mode === 'remove'" @click="mode = 'remove'">Remove</button>
    </div>

    <div class="position mono">
      <span>Your position</span><span class="v">{{ positionLabel }}</span>
    </div>

    <div class="model">
      <Chip :tone="badge.tone">{{ badge.text }}</Chip>
      <span class="mono dex">{{ pool.dex }}</span>
    </div>

    <template v-if="mode === 'add'">
      <div v-if="model === 'clmm'" class="range-box">
        <div>
          <div class="caps small">FEE TIER</div>
          <div class="chip-row gap">
            <button v-for="t in FEE_TIERS_BPS" :key="t" type="button" class="preset" :aria-pressed="feeTier === t" @click="feeTier = t">
              {{ bps(t) }}
            </button>
          </div>
        </div>

        <div>
          <div class="caps small">PRICE RANGE</div>
          <div class="chip-row gap">
            <button
              v-for="p in presetChips"
              :key="p.id"
              type="button"
              class="preset"
              :aria-pressed="activePreset === p.id"
              :disabled="!price"
              @click="p.id !== 'custom' && selectPreset(p.id)"
            >
              {{ p.name }}
            </button>
          </div>
        </div>

        <template v-if="price && range">
          <RangeBand :price="price" :range="range" :view="view" @min="setMin" @max="setMax" @freeze="frozen = $event" @step="stepBound" />

          <div class="bounds">
            <div class="bound">
              <label class="b-label mono" for="lp-min">MIN PRICE</label>
              <div class="b-row">
                <input id="lp-min" class="b-input mono" :value="range.minText" inputmode="decimal" data-testid="lp-min" @input="setMin(($event.target as HTMLInputElement).value)" />
                <div class="stepper">
                  <button type="button" aria-label="Raise min price 1%" @click="stepBound('min', 1)">▲</button>
                  <button type="button" aria-label="Lower min price 1%" @click="stepBound('min', -1)">▼</button>
                </div>
              </div>
            </div>
            <div class="bound">
              <label class="b-label mono" for="lp-max">MAX PRICE</label>
              <div class="b-row">
                <input id="lp-max" class="b-input mono" :value="range.maxText" inputmode="decimal" data-testid="lp-max" @input="setMax(($event.target as HTMLInputElement).value)" />
                <div class="stepper">
                  <button type="button" aria-label="Raise max price 1%" @click="stepBound('max', 1)">▲</button>
                  <button type="button" aria-label="Lower max price 1%" @click="stepBound('max', -1)">▼</button>
                </div>
              </div>
            </div>
          </div>

          <div class="kv">
            <div><span>Current price</span><span>{{ fmtPrice(price) }}</span></div>
            <div><span>Range width</span><span>{{ range.widthLabel }}</span></div>
            <div><span>Capital efficiency</span><span>{{ range.efficiency !== null ? '×' + range.efficiency : DASH }}</span></div>
            <div>
              <span>Status</span>
              <span :class="range.inRange || range.status === 'Always in range' ? 'ok' : 'warn'" data-testid="lp-status">{{ range.status }}</span>
            </div>
          </div>

          <div v-if="warning" class="warn-text" role="alert">{{ warning }}</div>
        </template>
        <div v-else class="notice" data-tone="muted">
          The backend does not report a spot price for this pool yet, so a range cannot be placed against it.
        </div>

        <p class="note">{{ note }}</p>
      </div>

      <div v-else class="note-box">{{ note }}</div>

      <label class="box">
        <span class="field-head"><span>{{ ctx.tokenSymbol.value }}</span></span>
        <input v-model="amountA" class="big-input mid" inputmode="decimal" placeholder="0.0" autocomplete="off" />
      </label>
      <label class="box">
        <span class="field-head"><span>{{ ctx.quoteSymbol.value }}</span></span>
        <input v-model="amountB" class="big-input mid" inputmode="decimal" placeholder="0.0" autocomplete="off" />
      </label>

      <div class="kv">
        <div><span>Pool share after</span><span>{{ DASH }}</span></div>
        <div><span>Fee tier / est. APR</span><span>{{ model === 'clmm' ? bps(feeTier) : bps(pool.feeTierBps) }} · {{ DASH }}</span></div>
      </div>
    </template>

    <template v-else>
      <div class="split">
        <div class="field-head"><label for="rm-pct">WITHDRAW</label><span class="v">{{ removePct }}% of position</span></div>
        <input id="rm-pct" v-model.number="removePct" type="range" min="5" max="100" step="5" />
        <div class="chip-row">
          <button v-for="p in [25, 50, 75, 100]" :key="p" type="button" class="preset grow" :aria-pressed="removePct === p" @click="removePct = p">{{ p }}%</button>
        </div>
      </div>
    </template>

    <button class="action" type="button" :data-tone="invalidRange ? 'blocked' : undefined" :disabled="invalidRange || (wallet.connected && !!unavailableReason)" @click="onAction">
      {{ buttonLabel }}
    </button>
    <div v-if="!invalidRange && unavailableReason" class="hint mono">{{ unavailableReason }}</div>
  </div>
</template>

<style scoped>
.position {
  display: flex;
  justify-content: space-between;
  gap: 10px;
  font-size: 11.5px;
  color: oklch(0.66 0.01 265);
  border: 1px solid var(--border-soft);
  border-radius: 10px;
  padding: 10px 12px;
}
.position .v {
  color: var(--fg-2);
  text-align: right;
}
.model {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.dex {
  font-size: 11.5px;
  color: oklch(0.66 0.01 265);
}
.range-box {
  display: flex;
  flex-direction: column;
  gap: 11px;
  border: 1px solid var(--border-soft);
  border-radius: 11px;
  padding: 12px;
}
.small {
  font-size: 10.5px;
}
.gap {
  margin-top: 7px;
}
.bounds {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  gap: 8px;
}
.bound {
  min-width: 0;
  border: 1px solid var(--border-soft);
  border-radius: 9px;
  padding: 8px 10px;
}
.b-label {
  font-size: 10px;
  color: var(--fg-muted);
}
.b-row {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-top: 4px;
}
.b-input {
  flex: 1;
  width: 100%;
  min-width: 0;
  background: transparent;
  border: none;
  outline: none;
  color: var(--fg);
  font-size: 14px;
  padding: 0;
}
.stepper {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.stepper button {
  width: 18px;
  height: 13px;
  line-height: 1;
  border: 1px solid var(--border-strong);
  border-radius: 4px;
  background: transparent;
  color: oklch(0.78 0.01 265);
  font-size: 8px;
  padding: 0;
}
.stepper button:hover {
  border-color: oklch(0.5 0.02 265);
}
.ok {
  color: var(--risk-safe-fg) !important;
}
.warn {
  color: var(--risk-watch-fg) !important;
}
.warn-text {
  font-size: 11.5px;
  line-height: 1.45;
  color: var(--warn-text);
}
.note {
  margin: 0;
  font-size: 11.5px;
  line-height: 1.5;
  color: var(--fg-muted);
  text-wrap: pretty;
}
.note-box {
  border: 1px solid var(--border-soft);
  border-radius: 11px;
  padding: 12px;
  font-size: 12.5px;
  line-height: 1.5;
  color: oklch(0.76 0.01 265);
  text-wrap: pretty;
}
.big-input.mid {
  font-size: 21px;
  margin-top: 5px;
}
.split {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.split .v {
  color: var(--fg);
}
.grow {
  flex: 1;
}
.hint {
  font-size: 11px;
  color: var(--fg-muted);
  text-align: center;
  line-height: 1.5;
}
</style>
