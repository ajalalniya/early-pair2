<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import RiskChip from '@/components/ui/RiskChip.vue'
import SwapTab from './SwapTab.vue'
import ZapTab from './ZapTab.vue'
import LiquidityTab from './LiquidityTab.vue'
import { useNow } from '@/composables/useNow'
import { chainMeta, dexscreenerUrl, poolKey } from '@/lib/chains'
import { age, buyerSellerRatio, money, num, ratio } from '@/lib/format'
import { useChainLookup } from '@/queries'
import { favoriteKeys, useFavoritesQuery, useToggleFavorite } from '@/queries/favorites'
import type { Pool } from '@/types/api'

const props = defineProps<{
  pool: Pool
  /** Open the Buy / Sell tab on a side; see SwapTab. */
  sideRequest?: { side: 'buy' | 'sell'; n: number }
  /** The wallet's balance of the traded token, in base units. */
  held?: string | null
}>()

type Tab = 'buy' | 'zap' | 'lp'
const route = useRoute()
// Deep links from the portfolio can open a specific tab.
const tab = ref<Tab>(route.query.tab === 'lp' || route.query.tab === 'zap' ? route.query.tab : 'buy')
watch(
  () => props.sideRequest?.n,
  () => {
    if (props.sideRequest) tab.value = 'buy'
  },
)
const tabs: { id: Tab; name: string }[] = [
  { id: 'buy', name: 'Buy / Sell' },
  { id: 'zap', name: 'Buy + LP' },
  { id: 'lp', name: 'Liquidity' },
]

const now = useNow()
const chains = useChainLookup()
const chain = computed(() => chainMeta(props.pool.chainId, chains.value.get(props.pool.chainId)))
const r = computed(() => buyerSellerRatio(props.pool.buyers1h, props.pool.sellers1h))
const honeypot = computed(() => r.value !== null && r.value > 8)
const key = computed(() => poolKey(props.pool.chainId, props.pool.address))
const dexUrl = computed(() => dexscreenerUrl(props.pool.chainId, props.pool.address, props.pool.pair))

// The same star as the pool rows, backed by the same favorites cache.
const favorites = useFavoritesQuery()
const starred = computed(() => favoriteKeys(favorites.data.value).has(key.value))
const toggleFavorite = useToggleFavorite()

function onTabKey(e: KeyboardEvent) {
  const i = tabs.findIndex((t) => t.id === tab.value)
  if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') {
    e.preventDefault()
    const next = tabs[(i + (e.key === 'ArrowRight' ? 1 : tabs.length - 1)) % tabs.length]!
    tab.value = next.id
    ;(document.getElementById(`tab-${next.id}`) as HTMLElement | null)?.focus()
  }
}

// A reorged pool stops being tradeable on screen the moment we hear about it.
const reorged = computed(() => props.pool.state === 'reorged')
</script>

<template>
  <section class="card summary" aria-labelledby="sel-pair" data-testid="trade-summary">
    <div class="top">
      <div class="min0">
        <div class="title">
          <button
            type="button"
            class="star"
            :class="{ on: starred }"
            :aria-pressed="starred"
            :aria-label="starred ? `Unstar ${pool.pair}` : `Star ${pool.pair}`"
            :title="starred ? 'Remove from favorite pairs' : 'Add to favorite pairs'"
            data-testid="summary-star"
            @click="toggleFavorite.mutate({ pool, star: !starred })"
          >
            {{ starred ? '★' : '☆' }}
          </button>
          <h2 id="sel-pair" class="pair mono">{{ pool.pair }}</h2>
        </div>
        <div class="meta mono">
          <span class="dot" :style="{ background: chain.dot }" />
          <span>{{ chain.name }} · {{ pool.dex }} · {{ age(pool.createdAt, now) }} old</span>
          <span v-if="pool.state === 'unconfirmed'" class="unconf">unconfirmed</span>
        </div>
      </div>
      <div class="top-side">
        <RiskChip :risk="pool.risk" size="md" />
        <a
          class="dex-link"
          :href="dexUrl"
          target="_blank"
          rel="noopener noreferrer"
          title="Open this pair on DexScreener"
          data-testid="summary-dexscreener"
        >
          DexScreener ↗
        </a>
      </div>
    </div>
    <div class="tiles">
      <div class="tile">
        <div class="t-label">LIQUIDITY</div>
        <div class="t-value">{{ money(pool.liquidityUsd) }}</div>
      </div>
      <div class="tile">
        <div class="t-label">VOL 1H</div>
        <div class="t-value">{{ money(pool.volume1hUsd) }}</div>
      </div>
      <div class="tile">
        <div class="t-label">UNIQUE BUYERS 1H</div>
        <div class="t-value buyers">{{ num(pool.buyers1h) }}</div>
      </div>
      <div class="tile">
        <div class="t-label">UNIQUE SELLERS 1H</div>
        <div class="t-value" :class="{ bad: honeypot }">{{ num(pool.sellers1h) }}</div>
      </div>
    </div>
    <div class="ratio mono">
      <span>Buyer : seller addresses</span>
      <span class="r" :class="{ bad: honeypot }">{{ ratio(pool.buyers1h, pool.sellers1h) }}</span>
    </div>
  </section>

  <section class="card trade">
    <div class="tabs" role="tablist" aria-label="Trade" @keydown="onTabKey">
      <button
        v-for="t in tabs"
        :id="`tab-${t.id}`"
        :key="t.id"
        type="button"
        role="tab"
        :aria-selected="tab === t.id"
        :aria-controls="`panel-${t.id}`"
        :tabindex="tab === t.id ? 0 : -1"
        @click="tab = t.id"
      >
        {{ t.name }}
      </button>
    </div>

    <div v-if="reorged" class="pad">
      <div class="notice" data-tone="danger">This pool was reorged out of the chain. It no longer exists and cannot be traded.</div>
    </div>
    <template v-else>
      <div v-show="tab === 'buy'" id="panel-buy" role="tabpanel" aria-labelledby="tab-buy">
        <SwapTab :key="key" :pool="pool" :active="tab === 'buy'" :side-request="sideRequest" :held="held" />
      </div>
      <div v-show="tab === 'zap'" id="panel-zap" role="tabpanel" aria-labelledby="tab-zap">
        <ZapTab :key="key" :pool="pool" :active="tab === 'zap'" />
      </div>
      <div v-show="tab === 'lp'" id="panel-lp" role="tabpanel" aria-labelledby="tab-lp">
        <LiquidityTab :key="key" :pool="pool" />
      </div>
    </template>
  </section>
</template>

<style scoped>
.summary {
  padding: 16px;
}
.top {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
}
.min0 {
  min-width: 0;
}
.top-side {
  flex: none;
  display: flex;
  flex-direction: column;
  align-items: flex-end;
  gap: 8px;
}
.dex-link {
  font-size: 11.5px;
  font-weight: 600;
  padding: 4px 9px;
  border-radius: 7px;
  border: 1px solid oklch(0.33 0.015 265);
  color: oklch(0.85 0.01 265);
  white-space: nowrap;
}
.dex-link:hover {
  border-color: oklch(0.5 0.02 265);
  color: var(--fg);
  filter: none;
}
.title {
  display: flex;
  align-items: flex-start;
  gap: 6px;
}
.star {
  flex: none;
  width: 26px;
  height: 26px;
  margin: -2px 0 0 -4px;
  display: grid;
  place-items: center;
  padding: 0;
  border: none;
  border-radius: 7px;
  background: transparent;
  color: var(--fg-faint);
  font-size: 18px;
  line-height: 1;
  cursor: pointer;
  transition:
    color 0.15s,
    background-color 0.15s;
}
.star:hover {
  color: var(--fg);
  background: var(--chip);
}
.star.on {
  color: oklch(0.84 0.15 85);
}
.pair {
  margin: 0;
  font-size: 17px;
  font-weight: 600;
  letter-spacing: -0.01em;
  overflow-wrap: anywhere;
}
.meta {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 7px;
  margin-top: 6px;
  font-size: 11.5px;
  color: oklch(0.66 0.01 265);
}
.dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
}
.unconf {
  color: var(--warn);
}
.tiles {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 10px;
  margin-top: 14px;
}
.t-label {
  font-size: 10.5px;
  font-family: var(--font-mono);
  color: var(--fg-muted);
  letter-spacing: 0.06em;
}
.t-value {
  font-size: 16px;
  font-weight: 600;
  margin-top: 4px;
  font-family: var(--font-mono);
}
.t-value.buyers {
  color: oklch(0.88 0.1 168);
}
.bad {
  color: oklch(0.78 0.15 25) !important;
}
.ratio {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-top: 10px;
  border: 1px solid var(--border-soft);
  border-radius: 10px;
  padding: 9px 11px;
  font-size: 11.5px;
  color: oklch(0.66 0.01 265);
}
.ratio .r {
  font-size: 13px;
  font-weight: 600;
  color: var(--fg);
}
.trade {
  overflow: hidden;
}
.tabs {
  display: flex;
  gap: 4px;
  padding: 8px;
  border-bottom: 1px solid var(--border-soft);
}
.tabs button {
  flex: 1;
  font-size: 13px;
  font-weight: 600;
  padding: 8px;
  border: none;
  border-radius: 9px;
  background: transparent;
  color: oklch(0.68 0.01 265);
}
.tabs button[aria-selected='true'] {
  background: var(--chip-active);
  color: var(--fg);
}
.pad {
  padding: 14px;
}
</style>
