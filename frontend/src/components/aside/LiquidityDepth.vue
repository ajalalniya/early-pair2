<script setup lang="ts">
/**
 * Where a concentrated pool's liquidity sits across price.
 *
 * Bars are liquidity (L, the pool's own unit), placed left to right by price
 * and drawn on a square-root scale: one position ten times the next would
 * otherwise flatten everything else to nothing. The bar holding the current
 * price is marked, because that is the liquidity a trade meets first — and a
 * pool whose price has run past every range has none, which is worth seeing.
 */
import { computed } from 'vue'
import { useLiquidityQuery } from '@/queries'
import { DASH, money, price as fmtPrice } from '@/lib/format'
import { errorMessage } from '@/lib/api'
import type { Pool } from '@/types/api'

const props = defineProps<{ pool: Pool; quoteSymbol: string; tokenSymbol: string }>()

const q = useLiquidityQuery(
  computed(() => ({ chainId: props.pool.chainId, address: props.pool.address, model: props.pool.model })),
)

const WIDTH = 320
const HEIGHT = 96

const view = computed(() => {
  const d = q.data.value
  if (!d || d.bins.length === 0) return null

  // Price runs left to right; a bin's width is its share of the price span, so
  // a wide range reads as wide rather than as one more bar.
  const lo = Math.min(...d.bins.map((b) => b.priceLower), d.price)
  const hi = Math.max(...d.bins.map((b) => b.priceUpper), d.price)
  // A log scale for x: price ranges here span orders of magnitude, and a
  // linear axis would squeeze every bin near zero into the same pixel.
  const l = (v: number) => Math.log10(Math.max(v, 1e-30))
  const x0 = l(lo)
  const span = Math.max(l(hi) - x0, 1e-9)
  const toX = (v: number) => ((l(v) - x0) / span) * WIDTH

  const tallest = Math.max(...d.bins.map((b) => b.liquidity))
  const toH = (L: number) => (tallest > 0 ? Math.max(2, Math.sqrt(L / tallest) * (HEIGHT - 8)) : 2)

  const bars = d.bins.map((b) => {
    const x = toX(b.priceLower)
    const w = Math.max(1.5, toX(b.priceUpper) - x)
    const h = toH(b.liquidity)
    return { ...b, x, w, h, y: HEIGHT - h }
  })
  return { bars, priceX: toX(d.price), lo, hi, d }
})

const activeBin = computed(() => q.data.value?.bins.find((b) => b.active) ?? null)

const subtitle = computed(() => {
  const d = q.data.value
  if (!d) return ''
  if (!d.bins.length) return 'No liquidity ranges found on chain.'
  const a = activeBin.value
  if (!a) return 'The price has moved past every range — no liquidity is active here.'
  return `Active range ${fmtPrice(a.priceLower)} – ${fmtPrice(a.priceUpper)} ${props.quoteSymbol}`
})

const hint = (b: { priceLower: number; priceUpper: number; amount0: number; amount1: number; active: boolean }) =>
  `${fmtPrice(b.priceLower)} – ${fmtPrice(b.priceUpper)} ${props.quoteSymbol}` +
  (b.active ? ' · holds the current price' : '')

const reason = computed(() => (q.isError.value ? errorMessage(q.error.value) : null))
</script>

<template>
  <section v-if="pool.model === 'clmm'" class="card depth" data-testid="liquidity-depth">
    <div class="head">
      <span class="caps">LIQUIDITY BY PRICE</span>
      <span class="mono price">{{ q.data.value ? fmtPrice(q.data.value.price) + ' ' + quoteSymbol : DASH }}</span>
    </div>

    <div v-if="q.isPending.value" class="state mono">Reading the pool's ticks…</div>
    <div v-else-if="reason" class="state mono">{{ reason }}</div>
    <div v-else-if="!view" class="state mono">No liquidity ranges found on chain.</div>

    <template v-else>
      <svg :viewBox="`0 0 ${WIDTH} ${HEIGHT}`" class="chart" role="img" :aria-label="`Liquidity by price for ${pool.pair}`">
        <rect
          v-for="b in view.bars"
          :key="`${b.tickLower}:${b.tickUpper}`"
          :x="b.x"
          :y="b.y"
          :width="b.w"
          :height="b.h"
          :class="{ bar: true, active: b.active }"
          :data-testid="b.active ? 'depth-bar-active' : 'depth-bar'"
        >
          <title>{{ hint(b) }}</title>
        </rect>
        <!-- The current price, so the bars read as "where the price is now". -->
        <line :x1="view.priceX" :x2="view.priceX" y1="0" :y2="HEIGHT" class="now" />
      </svg>
      <div class="axis mono">
        <span>{{ fmtPrice(view.lo) }}</span>
        <span>{{ fmtPrice(view.hi) }}</span>
      </div>
      <p class="note mono">
        {{ subtitle }}
        <span v-if="activeBin">
          · holds {{ money(activeBin.amount1) === DASH ? DASH : activeBin.amount1.toLocaleString(undefined, { maximumFractionDigits: 2 }) }}
          {{ quoteSymbol }} and {{ activeBin.amount0.toLocaleString(undefined, { maximumFractionDigits: 2 }) }} {{ tokenSymbol }}
        </span>
      </p>
      <p v-if="view.d.truncated" class="note mono dim">
        Liquidity continues past the edges of this chart; it is read around the current price.
      </p>
    </template>
  </section>
</template>

<style scoped>
.depth {
  padding: 12px 14px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 10px;
}
.price {
  font-size: 12px;
}
.chart {
  width: 100%;
  height: 96px;
  display: block;
  overflow: visible;
}
.bar {
  fill: color-mix(in oklch, var(--accent) 35%, transparent);
}
.bar.active {
  fill: var(--accent);
}
.now {
  stroke: var(--warn);
  stroke-width: 1;
  stroke-dasharray: 3 3;
}
.axis {
  display: flex;
  justify-content: space-between;
  font-size: 10px;
  color: var(--fg-muted);
}
.note {
  margin: 0;
  font-size: 11px;
  line-height: 1.5;
  color: var(--fg-muted);
}
.dim {
  color: var(--fg-3);
}
.state {
  padding: 18px 0;
  text-align: center;
  font-size: 11.5px;
  color: var(--fg-muted);
}
</style>
