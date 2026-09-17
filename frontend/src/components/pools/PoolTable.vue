<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useWindowVirtualizer } from '@tanstack/vue-virtual'
import ChainTag from '@/components/ui/ChainTag.vue'
import RiskChip from '@/components/ui/RiskChip.vue'
import { useMediaQuery, usePrefersReducedMotion } from '@/composables/useMediaQuery'
import { useNow } from '@/composables/useNow'
import { chainMeta, dexscreenerUrl, poolKey } from '@/lib/chains'
import { age, ageMinutes, buyerSellerRatio, money, num, pct } from '@/lib/format'
import { arrivals, leaving } from '@/queries/feed'
import type { Pool } from '@/types/api'

const props = withDefaults(
  defineProps<{
    rows: Pool[]
    total: number
    selectedKey: string | null
    filterSignature: string
    loading: boolean
    error: string | null
    hasMore: boolean
    loadingMore: boolean
    /** The feed of new pools, the starred pairs above it, or address search results. */
    variant?: 'feed' | 'favorites' | 'search'
    /** Shown instead of the feed's own wording when there are no rows. */
    emptyText?: string
    /** Keys of starred pairs, for the star on each row. */
    favoriteKeys?: Set<string>
  }>(),
  { variant: 'feed', favoriteKeys: () => new Set<string>() },
)
const emit = defineEmits<{ select: [pool: Pool]; loadMore: []; retry: []; toggleFavorite: [pool: Pool] }>()

const isFavorites = computed(() => props.variant === 'favorites')
const isSearch = computed(() => props.variant === 'search')
const label = computed(() => (isFavorites.value ? 'Favorite pairs' : isSearch.value ? 'Search results' : 'New pools'))

const now = useNow()
const compact = useMediaQuery('(max-width: 819px)')
const reducedMotion = usePrefersReducedMotion()
const rowHeight = computed(() => (compact.value ? 172 : 62))

const listEl = ref<HTMLElement | null>(null)
const scrollMargin = ref(0)

function measureMargin() {
  if (listEl.value) scrollMargin.value = listEl.value.getBoundingClientRect().top + window.scrollY
}

const keyOf = (p: Pool) => poolKey(p.chainId, p.address)

const virtualizer = useWindowVirtualizer(
  computed(() => ({
    count: props.rows.length,
    estimateSize: () => rowHeight.value,
    overscan: 8,
    scrollMargin: scrollMargin.value,
    getItemKey: (i: number) => (props.rows[i] ? keyOf(props.rows[i]!) : i),
  })),
)

const items = computed(() => virtualizer.value.getVirtualItems())
const totalSize = computed(() => virtualizer.value.getTotalSize())

// ------------------------------------------------------------ live inserts

const newAbove = ref(0)
let prevTop: string | null = null
let prevSignature = props.filterSignature

function scrolledIntoList(): boolean {
  return window.scrollY > scrollMargin.value - 120
}

watch(
  () => [props.rows[0] ? keyOf(props.rows[0]) : null, props.filterSignature] as const,
  async ([top, sig]) => {
    if (props.variant !== 'feed') return
    const sameView = sig === prevSignature
    prevSignature = sig
    const oldTop = prevTop
    prevTop = top
    if (!sameView) {
      newAbove.value = 0
      return
    }
    if (!oldTop || !top || oldTop === top) return
    const shift = props.rows.findIndex((p) => keyOf(p) === oldTop)
    if (shift <= 0) return
    if (scrolledIntoList()) {
      // Hold the user's place: the rows they are reading must not move.
      await nextTick()
      window.scrollBy({ top: shift * rowHeight.value, behavior: 'instant' as ScrollBehavior })
      newAbove.value += shift
    }
  },
  { flush: 'post' },
)

function onScroll() {
  if (newAbove.value && window.scrollY <= scrollMargin.value - 60) newAbove.value = 0
  // Infinite scroll: fetch the next page as the end comes into view.
  const last = items.value[items.value.length - 1]
  if (last && props.hasMore && !props.loadingMore && last.index >= props.rows.length - 10) emit('loadMore')
}

function jumpToTop() {
  newAbove.value = 0
  window.scrollTo({ top: Math.max(0, scrollMargin.value - 180), behavior: reducedMotion.value ? 'auto' : 'smooth' })
}

let ro: ResizeObserver | null = null
onMounted(() => {
  measureMargin()
  window.addEventListener('scroll', onScroll, { passive: true })
  window.addEventListener('resize', measureMargin)
  ro = new ResizeObserver(measureMargin)
  ro.observe(document.body)
})
onBeforeUnmount(() => {
  window.removeEventListener('scroll', onScroll)
  window.removeEventListener('resize', measureMargin)
  ro?.disconnect()
})

watch(compact, () => nextTick(() => virtualizer.value.measure()))

// ------------------------------------------------------------ rows

function view(p: Pool) {
  const key = keyOf(p)
  const mins = ageMinutes(p.createdAt, now.value)
  const r = buyerSellerRatio(p.buyers1h, p.sellers1h)
  const arrivedAt = arrivals.value.get(key)
  const pending = p.tradersPending ?? 0
  const starred = props.favoriteKeys.has(key)
  return {
    key,
    pool: p,
    tag: chainMeta(p.chainId).tag,
    age: age(p.createdAt, now.value),
    fresh: mins !== null && mins <= 10,
    liq: money(p.liquidityUsd),
    vol: money(p.volume1hUsd),
    buyers: num(p.buyers1h),
    sellers: num(p.sellers1h),
    // Traders still being looked up: the counts are a floor until this is zero.
    partialTraders: pending > 0 && p.buyers1h !== null,
    tradersTitle: pending > 0 ? `${num(pending)} swaps still being attributed to a trader — counts will rise` : undefined,
    starred,
    removed: !!p.removedAt,
    added: !!p.importedAt,
    honeypot: r !== null && r > 8,
    change: pct(p.priceChange1h),
    changeTone: p.priceChange1h === null ? '' : p.priceChange1h >= 0 ? 'up' : 'down',
    unconfirmed: p.state === 'unconfirmed',
    leaving: leaving.value.has(key),
    flash: arrivedAt !== undefined && Date.now() - arrivedAt < 1500,
    selected: props.selectedKey === key,
    dexUrl: dexscreenerUrl(p.chainId, p.address, p.pair),
  }
}

const rowRefs = new Map<string, HTMLElement>()
function setRowRef(key: string, el: unknown) {
  if (el instanceof HTMLElement) rowRefs.set(key, el)
  else rowRefs.delete(key)
}

async function focusIndex(i: number) {
  const target = props.rows[i]
  if (!target) return
  virtualizer.value.scrollToIndex(i, { align: 'auto' })
  await nextTick()
  requestAnimationFrame(() => rowRefs.get(keyOf(target))?.focus({ preventScroll: true }))
}

function onRowKey(e: KeyboardEvent, index: number, p: Pool) {
  if (e.target !== e.currentTarget) return
  if (e.key === 'Enter' || e.key === ' ') {
    e.preventDefault()
    emit('select', p)
  } else if (e.key === 'ArrowDown') {
    e.preventDefault()
    void focusIndex(index + 1)
  } else if (e.key === 'ArrowUp') {
    e.preventDefault()
    void focusIndex(index - 1)
  } else if (e.key === 'Home') {
    e.preventDefault()
    void focusIndex(0)
  } else if (e.key === 'End') {
    e.preventDefault()
    void focusIndex(props.rows.length - 1)
  }
}
</script>

<template>
  <div
    class="table card"
    :class="{ favorites: isFavorites }"
    role="table"
    :aria-label="label"
    :aria-rowcount="rows.length"
    :data-testid="isFavorites ? 'favorites-table' : isSearch ? 'search-table' : 'pools-table'"
  >
    <div v-if="isSearch" class="caption">
      <span class="caption-title">SEARCH RESULTS</span>
      <span class="caption-note">all chains, including pairs removed from the feed</span>
    </div>
    <div v-if="isFavorites" class="caption">
      <span class="caption-title"><span class="caption-star" aria-hidden="true">★</span>FAVORITE PAIRS</span>
      <span class="caption-note">watched live until unstarred</span>
    </div>
    <div class="head grid" role="row">
      <div role="columnheader">POOL</div>
      <div role="columnheader" class="r">AGE</div>
      <div role="columnheader" class="r">LIQUIDITY</div>
      <div role="columnheader" class="r">VOL 1H</div>
      <div role="columnheader" class="r">BUYERS / SELLERS</div>
      <div role="columnheader" class="r">PRICE 1H</div>
      <div role="columnheader" class="c">RISK</div>
      <div role="columnheader"><span class="visually-hidden">Trade</span></div>
    </div>

    <div class="new-pill-anchor">
      <button v-if="newAbove > 0 && variant === 'feed'" class="new-pill" type="button" @click="jumpToTop">
        {{ newAbove }} new {{ newAbove === 1 ? 'pair' : 'pairs' }} ↑
      </button>
    </div>
    <span class="visually-hidden" aria-live="polite">{{ newAbove > 0 ? `${newAbove} new pairs above` : '' }}</span>

    <div v-if="error && rows.length === 0" class="state">
      <span>Could not load pools: {{ error }}</span>
      <button class="btn btn-sm" type="button" @click="emit('retry')">Retry</button>
    </div>
    <div v-else-if="loading && rows.length === 0" class="skeletons" aria-busy="true">
      <div v-for="i in 8" :key="i" class="skeleton grid">
        <span class="bar w60" /><span class="bar" /><span class="bar" /><span class="bar" /><span class="bar" /><span class="bar" /><span class="bar" /><span class="bar" />
      </div>
    </div>
    <div v-else-if="rows.length === 0" class="state">
      <span>{{ emptyText ?? (total === 0 ? 'No pools indexed yet — new pairs appear here the moment they deploy.' : 'No pools match these filters.') }}</span>
    </div>

    <div ref="listEl" class="body" :style="{ height: rows.length ? totalSize + 'px' : '0px' }">
      <div
        v-for="item in items"
        :key="String(item.key)"
        class="slot"
        :style="{ height: item.size + 'px', transform: `translateY(${item.start - virtualizer.options.scrollMargin}px)` }"
      >
        <template v-if="rows[item.index]">
          <div
            v-for="v in [view(rows[item.index]!)]"
            :key="v.key"
            :ref="(el) => setRowRef(v.key, el)"
            class="row grid"
            role="row"
            tabindex="0"
            :aria-selected="v.selected"
            :aria-rowindex="item.index + 1"
            :class="{ selected: v.selected, flash: v.flash, leaving: v.leaving }"
            :data-testid="`pool-row-${v.key}`"
            @click="emit('select', v.pool)"
            @keydown="onRowKey($event, item.index, v.pool)"
          >
            <div role="cell" class="pair-cell">
              <button
                type="button"
                class="star"
                :class="{ on: v.starred }"
                :aria-pressed="v.starred"
                :aria-label="v.starred ? `Unstar ${v.pool.pair}` : `Star ${v.pool.pair}`"
                :title="v.starred ? 'Remove from favorite pairs' : 'Add to favorite pairs'"
                :data-testid="`star-${v.key}`"
                @click.stop="emit('toggleFavorite', v.pool)"
                @keydown.stop
              >
                {{ v.starred ? '★' : '☆' }}
              </button>
              <ChainTag :chain-id="v.pool.chainId" />
              <div class="pair-text">
                <div class="pair">
                  <span class="pair-name">{{ v.pool.pair }}</span>
                  <span v-if="v.unconfirmed" class="unconfirmed" title="unconfirmed — waiting for block confirmations" aria-label="unconfirmed" />
                </div>
                <div class="dex">
                  <span class="dex-name">{{ v.pool.dex }}</span>
                  <span v-if="v.removed" class="removed-tag" title="Removed from the feed: under the liquidity/volume bar at 3 minutes">REMOVED</span>
                  <span
                    v-if="v.added"
                    class="added-tag"
                    data-testid="added-tag"
                    title="Added by address: this pair was not in the feed, so its figures start when it was added, not at its launch"
                  >ADDED</span>
                </div>
              </div>
            </div>
            <div role="cell" class="num age" :class="{ fresh: v.fresh }"><span class="lbl">AGE</span>{{ v.age }}</div>
            <div role="cell" class="num"><span class="lbl">LIQUIDITY</span>{{ v.liq }}</div>
            <div role="cell" class="num vol"><span class="lbl">VOL 1H</span>{{ v.vol }}</div>
            <div role="cell" class="num bs" :title="v.tradersTitle">
              <span class="lbl">BUYERS / SELLERS</span>
              <span class="buyers">{{ v.buyers }}<span v-if="v.partialTraders" class="floor">+</span></span><span class="slash">/</span
              ><span class="sellers" :class="{ honeypot: v.honeypot }">{{ v.sellers }}<span v-if="v.partialTraders" class="floor">+</span></span>
            </div>
            <div role="cell" class="num change" :data-tone="v.changeTone"><span class="lbl">PRICE 1H</span>{{ v.change }}</div>
            <div role="cell" class="risk-cell"><RiskChip :risk="v.pool.risk" /></div>
            <div role="cell" class="trade-cell">
              <a class="trade" :href="v.dexUrl" target="_blank" rel="noopener noreferrer" title="Open on DexScreener" @click.stop @keydown.stop>Trade ↗</a>
            </div>
          </div>
        </template>
      </div>
    </div>

    <div v-if="isFavorites" class="foot">
      <span>{{ rows.length }} starred {{ rows.length === 1 ? 'pair' : 'pairs' }}</span>
    </div>
    <div v-else-if="isSearch" class="foot">
      <span v-if="rows.length">{{ rows.length }} matching {{ rows.length === 1 ? 'pair' : 'pairs' }}{{ rows.length >= 20 ? ' — type more of the address to narrow it' : '' }}</span>
    </div>
    <div v-else class="foot">
      <span>{{ rows.length }} of {{ total }} pools shown</span>
      <span v-if="loadingMore" class="more">loading more…</span>
      <button v-else-if="hasMore" class="link" type="button" @click="emit('loadMore')">Load older pools</button>
    </div>
  </div>
</template>

<style scoped>
.table {
  overflow: clip;
  position: relative;
}
.grid {
  display: grid;
  grid-template-columns: minmax(165px, 2fr) 62px 1fr 1fr 1.1fr 1fr 92px 116px;
  gap: 12px;
  align-items: center;
}
.head {
  padding: 11px 16px;
  border-bottom: 1px solid var(--chip-2);
  font-family: var(--font-mono);
  font-size: 10.5px;
  letter-spacing: 0.07em;
  color: var(--fg-muted);
}
.r {
  text-align: right;
}
.c {
  text-align: center;
}
.body {
  position: relative;
  width: 100%;
}
.slot {
  position: absolute;
  top: 0;
  left: 0;
  width: 100%;
}
.row {
  height: 100%;
  padding: 0 16px;
  border-bottom: 1px solid var(--border-faint);
  cursor: pointer;
  transition:
    background-color 0.15s,
    opacity 0.4s;
  outline-offset: -2px;
}
.row:hover {
  background: oklch(0.215 0.013 265);
}
.row.selected {
  background: var(--row-hover);
}
.row.flash {
  animation: rowFlash 0.4s ease-out;
}
.row.leaving {
  opacity: 0;
  pointer-events: none;
}
@keyframes rowFlash {
  from {
    background-color: color-mix(in oklch, var(--accent) 22%, transparent);
  }
  to {
    background-color: transparent;
  }
}
.pair-cell {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}
.star {
  flex: none;
  width: 22px;
  height: 22px;
  display: grid;
  place-items: center;
  padding: 0;
  border: none;
  border-radius: 6px;
  background: transparent;
  color: var(--fg-faint);
  font-size: 15px;
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
.caption {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
  padding: 12px 16px 2px;
  font-family: var(--font-mono);
}
.caption-title {
  font-size: 11.5px;
  font-weight: 600;
  letter-spacing: 0.07em;
  color: var(--fg);
}
.caption-star {
  margin-right: 6px;
  color: oklch(0.84 0.15 85);
}
.caption-note {
  font-size: 11px;
  color: var(--fg-muted);
}
.floor {
  color: var(--fg-muted);
}
.added-tag {
  flex: none;
  font-family: var(--font-mono);
  font-size: 9.5px;
  font-weight: 600;
  letter-spacing: 0.05em;
  padding: 1px 5px;
  border-radius: 4px;
  color: var(--accent);
  border: 1px solid color-mix(in oklch, var(--accent) 45%, transparent);
}
.removed-tag {
  flex: none;
  font-family: var(--font-mono);
  font-size: 9.5px;
  font-weight: 600;
  letter-spacing: 0.05em;
  padding: 1px 5px;
  border-radius: 4px;
  color: var(--warn);
  border: 1px solid color-mix(in oklch, var(--warn) 45%, transparent);
}
.pair-text {
  min-width: 0;
}
.pair {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
}
.pair-name {
  font-family: var(--font-mono);
  font-size: 13px;
  font-weight: 600;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.unconfirmed {
  flex: none;
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--warn);
  animation: pulseDot 1.4s infinite;
}
.dex {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
  font-size: 11px;
  color: var(--fg-muted);
  font-family: var(--font-mono);
  margin-top: 2px;
}
.dex-name {
  min-width: 0;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.num {
  text-align: right;
  font-family: var(--font-mono);
  font-size: 13px;
  white-space: nowrap;
}
.lbl {
  display: none;
}
.age {
  color: var(--fg-3);
}
.age.fresh {
  color: var(--age-fresh);
}
.vol {
  color: oklch(0.76 0.01 265);
}
.bs {
  display: flex;
  justify-content: flex-end;
  align-items: baseline;
  gap: 4px;
}
.buyers {
  color: oklch(0.86 0.09 168);
}
.slash {
  color: var(--fg-faint);
}
.sellers {
  color: var(--fg-3);
}
.sellers.honeypot {
  color: var(--honeypot);
}
.change {
  font-weight: 500;
  color: var(--fg-3);
}
.change[data-tone='up'] {
  color: var(--gain);
}
.change[data-tone='down'] {
  color: var(--loss);
}
.risk-cell {
  display: flex;
  justify-content: center;
}
.trade-cell {
  display: flex;
  justify-content: flex-end;
}
.trade {
  font-size: 11.5px;
  font-weight: 600;
  padding: 5px 10px;
  border-radius: 7px;
  border: 1px solid oklch(0.33 0.015 265);
  color: oklch(0.85 0.01 265);
  white-space: nowrap;
}
.trade:hover {
  border-color: oklch(0.5 0.02 265);
  color: var(--fg);
  filter: none;
}
.new-pill-anchor {
  position: sticky;
  top: calc(var(--header-h) + 10px);
  height: 0;
  z-index: 5;
  display: flex;
  justify-content: center;
}
.new-pill {
  margin-top: 8px;
  font-family: var(--font-mono);
  font-size: 11.5px;
  font-weight: 600;
  padding: 6px 12px;
  border-radius: 999px;
  border: none;
  background: var(--accent);
  color: var(--on-accent);
}
.state {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 12px;
  flex-wrap: wrap;
  padding: 36px 16px;
  font-family: var(--font-mono);
  font-size: 12px;
  color: var(--fg-muted);
  text-align: center;
}
.skeleton {
  padding: 22px 16px;
  border-bottom: 1px solid var(--border-faint);
}
.skeleton .bar {
  display: block;
  height: 10px;
  border-radius: 4px;
  background: var(--chip);
  animation: pulseDot 1.6s infinite;
}
.skeleton .w60 {
  width: 60%;
}
.foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 12px 16px;
  font-size: 12px;
  color: var(--fg-muted);
  font-family: var(--font-mono);
}
.link {
  border: none;
  background: transparent;
  color: var(--accent);
  font-family: var(--font-mono);
  font-size: 12px;
  padding: 0;
}

/* Stacked cards below 820px: pair + chain, a two-column metric grid, then risk and Trade. */
@media (max-width: 819px) {
  .head {
    display: none;
  }
  .row.grid {
    grid-template-columns: 1fr 1fr;
    grid-template-areas:
      'pair pair'
      'age liq'
      'vol bs'
      'chg chg'
      'risk trade';
    gap: 6px 14px;
    padding: 12px 14px;
    align-content: center;
  }
  .pair-cell {
    grid-area: pair;
  }
  .age {
    grid-area: age;
  }
  .row .num:nth-child(3) {
    grid-area: liq;
  }
  .vol {
    grid-area: vol;
  }
  .bs {
    grid-area: bs;
  }
  .change {
    grid-area: chg;
  }
  .risk-cell {
    grid-area: risk;
    justify-content: flex-start;
  }
  .trade-cell {
    grid-area: trade;
  }
  .num,
  .bs {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    gap: 6px;
    font-size: 12.5px;
  }
  .bs .buyers {
    margin-left: auto;
  }
  .lbl {
    display: inline;
    font-size: 10px;
    letter-spacing: 0.05em;
    color: var(--fg-muted);
  }
  .skeleton.grid {
    grid-template-columns: 1fr 1fr;
  }
}
</style>
