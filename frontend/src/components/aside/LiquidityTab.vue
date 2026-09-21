<script setup lang="ts">
import { computed, ref, toRef } from 'vue'
import Chip from '@/components/ui/Chip.vue'
import RangeBand from './RangeBand.vue'
import { useTradeContext } from '@/composables/useTradeContext'
import { ApiError } from '@/lib/api'
import { poolKey } from '@/lib/chains'
import { estimateFees } from '@/lib/feeEstimate'
import { DASH, bps, money, moneyFine, price as fmtPrice } from '@/lib/format'
import { FEE_TIERS_BPS, RANGE_PRESETS, axisView, computeRange, step, type PresetId, type RangeView } from '@/lib/range'
import { useLiquidityQuery, usePositionsQuery } from '@/queries'
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

// The backend does not put a spot price on a pool, but a concentrated pool's
// own state carries one, and the depth chart already reads it. Sharing that
// query is what lets a range be placed at all — and its liquidity is what the
// fee estimate below is measured against. Both come from one request: the
// depth chart beside this panel asks for the same key.
const liq = useLiquidityQuery(
  computed(() => ({ chainId: props.pool.chainId, address: props.pool.address, model: props.pool.model })),
)
const price = computed(() => {
  const p = props.pool.price ?? liq.data.value?.price ?? null
  return p !== null && p > 0 ? p : null
})
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

// ------------------------------------------------------------ fee estimate
// What the chosen range would earn. The pool's fee tier is the pool's, not the
// chips above: those pick which pool you would add to, and the only volume and
// liquidity known here belong to this one.
const poolFeeBps = computed(() => liq.data.value?.feeBps ?? props.pool.feeTierBps ?? null)
const depositText = ref('1')
const depositUsd = computed(() => {
  const v = parseFloat(depositText.value.trim().replace(/,/g, ''))
  return Number.isFinite(v) && v > 0 ? v : null
})

const estimate = computed(() => {
  const d = liq.data.value
  const r = range.value
  const t0 = props.pool.token0
  const t1 = props.pool.token1
  if (!d || !r?.valid || !price.value || !depositUsd.value || !t0 || !t1) return null
  return estimateFees({
    price: price.value,
    min: r.min,
    max: r.max,
    depositUsd: depositUsd.value,
    quoteUsd: d.quoteUsd,
    activeLiquidity: d.liquidity,
    decimals0: t0.decimals,
    decimals1: t1.decimals,
    volume1hUsd: props.pool.volume1hUsd,
    feeBps: poolFeeBps.value,
  })
})

/** Why there is no number, in the pool's terms rather than as a blank. */
const estimateUnknown = computed(() => {
  if (estimate.value) return null
  if (liq.isPending.value) return "Reading the pool's liquidity…"
  if (!depositUsd.value) return 'Enter an amount to deposit.'
  if (!price.value) return 'This pool does not report a price, so a range cannot be measured against it.'
  if (props.pool.volume1hUsd === null) return 'No volume recorded in the last hour, so there is nothing to project.'
  if (poolFeeBps.value === null) return "This pool's fee tier is not known."
  if (liq.data.value && liq.data.value.quoteUsd === null)
    return `${ctx.quoteSymbol.value} has no established dollar price on this chain, so a dollar deposit cannot be sized.`
  if (liq.data.value && liq.data.value.liquidity <= 0) return 'The pool has no liquidity at its current price.'
  return 'Not enough is known about this pool to estimate fees.'
})

/**
 * Fees on a dollar are fractions of a cent, and rounding them to three decimals
 * reads as zero — which is not what a dollar in a busy pool earns. Keep three
 * significant digits below a cent, and ordinary money above it.
 */
const feeLabel = computed(() => {
  const v = estimate.value?.feeUsd1h
  if (v === undefined) return DASH
  if (v === 0) return '$0'
  if (v < 0.01) return `$${Number(v.toPrecision(3))}`
  return moneyFine(v)
})

/** A young pair can imply four figures of APR; say it in multiples past that. */
const aprLabel = computed(() => {
  const a = estimate.value?.aprPct
  if (a === undefined) return DASH
  if (a >= 100_000) return `${Math.round(a / 100).toLocaleString('en-US')}x`
  if (a >= 100) return `${Math.round(a).toLocaleString('en-US')}%`
  return `${a.toFixed(1)}%`
})

/** A dollar in a live pool is a very small share; three digits keep it readable. */
const shareLabel = computed(() => {
  const s = estimate.value?.share
  if (s === undefined) return DASH
  const p = s * 100
  if (p >= 1) return `${p.toFixed(1)}%`
  if (p >= 0.01) return `${p.toFixed(2)}%`
  return `${Number(p.toPrecision(2))}%`
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

          <div class="est" data-testid="lp-fee-estimate">
            <div class="est-head">
              <span class="caps small">FEES IN 1 HOUR</span>
              <label class="dep">
                <span>on</span>
                <span class="sign">$</span>
                <input
                  class="dep-input mono"
                  :value="depositText"
                  inputmode="decimal"
                  aria-label="Deposit in dollars"
                  data-testid="lp-deposit"
                  @input="depositText = ($event.target as HTMLInputElement).value"
                />
              </label>
            </div>

            <template v-if="estimate">
              <div class="est-figure">
                <strong class="mono" :class="estimate.inRange ? 'ok' : 'warn'" data-testid="lp-fee-1h">
                  {{ feeLabel }}
                </strong>
                <span v-if="estimate.inRange" class="est-side mono">
                  ≈ {{ aprLabel }} a year · {{ shareLabel }} of the liquidity in range
                </span>
                <span v-else class="est-side mono">price is outside this range, so it earns nothing</span>
              </div>
              <p class="note">
                Assumes the next hour trades like the last one ({{ money(estimate.volume1hUsd) }}) at this pool's
                {{ bps(poolFeeBps) }} fee, and that the price stays in your range. Fees only — a position also moves in
                value as the price does.
              </p>
            </template>
            <p v-else class="note">{{ estimateUnknown }}</p>
          </div>
        </template>
        <div v-else class="notice" data-tone="muted">
          {{
            liq.isPending.value
              ? "Reading the pool's price from chain…"
              : 'No price could be read for this pool, so a range cannot be placed against it.'
          }}
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
        <div><span>Pool share after</span><span>{{ estimate?.inRange ? shareLabel : DASH }}</span></div>
        <div>
          <span>Fee tier / est. APR</span>
          <span>{{ model === 'clmm' ? bps(feeTier) : bps(pool.feeTierBps) }} · {{ estimate?.inRange ? aprLabel : DASH }}</span>
        </div>
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
.est {
  border-top: 1px solid var(--border-soft);
  padding-top: 10px;
  display: flex;
  flex-direction: column;
  gap: 7px;
}
.est-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
}
.dep {
  display: flex;
  align-items: baseline;
  gap: 4px;
  font-size: 11px;
  color: var(--fg-muted);
}
.dep .sign {
  color: var(--fg-2);
}
.dep-input {
  width: 62px;
  background: transparent;
  border: none;
  border-bottom: 1px solid var(--border-strong);
  outline: none;
  color: var(--fg);
  font-size: 12.5px;
  padding: 0 0 1px;
}
.dep-input:focus {
  border-bottom-color: var(--accent);
}
.est-figure {
  display: flex;
  align-items: baseline;
  gap: 8px;
  flex-wrap: wrap;
}
.est-figure strong {
  font-size: 19px;
  font-weight: 600;
}
.est-side {
  font-size: 11px;
  color: var(--fg-muted);
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
