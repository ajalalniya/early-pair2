<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter, type LocationQueryRaw } from 'vue-router'
import FilterBar from '@/components/pools/FilterBar.vue'
import PoolTable from '@/components/pools/PoolTable.vue'
import SearchBar from '@/components/pools/SearchBar.vue'
import FundsCard from '@/components/aside/FundsCard.vue'
import TradePanel from '@/components/aside/TradePanel.vue'
import FraudCard from '@/components/aside/FraudCard.vue'
import { api, errorMessage } from '@/lib/api'
import { parsePoolKey, poolKey } from '@/lib/chains'
import { matchesRisk, type RiskFilter } from '@/lib/risk'
import { activeCount, COLUMN_KEYS, encodeRange, filtersFromQuery, matchesColumns, type ColumnFilters } from '@/lib/columnFilters'
import { ALL_DEXES, dexFromQuery, dexOptions, matchesDex } from '@/lib/dexFilter'
import { useNow } from '@/composables/useNow'
import { useQueryClient } from '@tanstack/vue-query'
import { useChainsQuery, usePoolQuery } from '@/queries'
import { qk } from '@/queries/keys'
import { favoriteKeys, useFavoritesQuery, useToggleFavorite } from '@/queries/favorites'
import { flattenFeed, leaving, useFeedQuery } from '@/queries/feed'
import { useSearchPoolsQuery } from '@/queries/search'
import { searchTerm } from '@/lib/search'
import type { Pool } from '@/types/api'

const route = useRoute()
const router = useRouter()

const RISKS: RiskFilter[] = ['all', 'safe', 'watch', 'danger']
const chain = computed(() => (typeof route.query.chain === 'string' && route.query.chain ? route.query.chain : 'all'))
const risk = computed<RiskFilter>(() => {
  const r = route.query.risk
  return typeof r === 'string' && (RISKS as string[]).includes(r) ? (r as RiskFilter) : 'all'
})
const selParam = computed(() => (typeof route.query.sel === 'string' ? route.query.sel : null))
const columns = computed<ColumnFilters>(() => filtersFromQuery(route.query))
const columnsActive = computed(() => activeCount(columns.value) > 0)
// Age filters move with the clock; recheck them as rows age.
const now = useNow()

// ------------------------------------------------------------ address search
// The box writes `q` to the URL after a short pause, like the other filters,
// so a search can be linked and survives a reload.
const qParam = computed(() => (typeof route.query.q === 'string' ? route.query.q : ''))
const searchText = ref(qParam.value)
const searching = computed(() => qParam.value.trim() !== '')
const term = computed(() => searchTerm(qParam.value))
let searchTimer: number | undefined
// The box writes the URL and the URL writes the box; without this flag the
// echo writes the URL a second time, and that write carries `sel: undefined`,
// so a selection made while clearing the search is dropped 250ms later.
let syncingFromUrl = false
watch(searchText, (v) => {
  if (syncingFromUrl) {
    syncingFromUrl = false
    return
  }
  window.clearTimeout(searchTimer)
  searchTimer = window.setTimeout(() => writeQuery({ q: v.trim() || undefined, sel: undefined }), 250)
})
watch(qParam, (v) => {
  if (v.trim() !== searchText.value.trim()) {
    syncingFromUrl = true
    searchText.value = v
  }
})

const search = useSearchPoolsQuery(term)
const searchRows = computed(() => (term.value ? (search.data.value ?? []) : []))
const searchHint = computed(() =>
  searching.value && !term.value ? 'Enter a pair address: 0x followed by at least 4 hex characters.' : null,
)
const searchEmpty = computed(() => (term.value ? `No pair found with an address starting ${term.value}.` : 'Nothing to search for yet.'))
const searchError = computed(() => (search.isError.value ? errorMessage(search.error.value) : null))

// ------------------------------------------------------------ adding a pool
// The feed only holds pools created while the backend was watching, so a pair
// from before that is not listed and cannot be traded. Searching its address
// finds nothing; this is the way to add it, by reading it off the chain.
const qc = useQueryClient()
const addChain = ref('')
const adding = ref(false)
const addError = ref<string | null>(null)
const addedNote = ref<string | null>(null)

const fullAddress = computed(() => {
  const t = term.value ?? ''
  return /^0x[0-9a-f]{40}$/.test(t) || /^0x[0-9a-f]{64}$/.test(t) ? t : null
})
const canAddPool = computed(
  () => !!fullAddress.value && !search.isPending.value && !search.isError.value && searchRows.value.length === 0,
)
// Chains that actually index pools; the selected chain filter is the default.
const addChains = computed(() => (chains.data.value ?? []).filter((c) => c.ingesting))
const addChainId = computed(() => addChain.value || (chain.value !== 'all' ? chain.value : (addChains.value[0]?.id ?? '')))

async function addPool() {
  const address = fullAddress.value
  if (!address || !addChainId.value || adding.value) return
  adding.value = true
  addError.value = null
  addedNote.value = null
  try {
    const { pool, added } = await api.importPool(addChainId.value, address)
    await qc.invalidateQueries({ queryKey: qk.favorites })
    addedNote.value = added ? `Added ${pool.pair}. It is in Favorite pairs, tagged ADDED.` : `${pool.pair} was already listed.`
    // Clear the search and select what was just added, in one write: the box
    // follows the URL, so setting it here would schedule a second write.
    window.clearTimeout(searchTimer)
    writeQuery({ q: undefined, sel: poolKey(pool.chainId, pool.address) })
  } catch (err) {
    addError.value = errorMessage(err)
  } finally {
    adding.value = false
  }
}

const chains = useChainsQuery()
const dex = computed(() => dexFromQuery(route.query.dex, dexOptions(chains.data.value, chain.value)))
const filterSignature = computed(() =>
  [chain.value, risk.value, dex.value, ...COLUMN_KEYS.map((k) => encodeRange(columns.value[k]) ?? '')].join('|'),
)
const feed = useFeedQuery()

const favorites = useFavoritesQuery()
const favoriteRows = computed(() => favorites.data.value ?? [])
const starred = computed(() => favoriteKeys(favorites.data.value))
const toggle = useToggleFavorite()

function toggleFavorite(p: Pool) {
  toggle.mutate({ pool: p, star: !starred.value.has(poolKey(p.chainId, p.address)) })
}

const all = computed(() => flattenFeed(feed.data.value))
// A starred pair moves out of the feed into the favorites section.
const rows = computed(() =>
  all.value.filter(
    (p) =>
      !starred.value.has(poolKey(p.chainId, p.address)) &&
      (chain.value === 'all' || p.chainId === chain.value) &&
      matchesRisk(p, risk.value) &&
      matchesDex(p, dex.value) &&
      (!columnsActive.value || matchesColumns(p, columns.value, now.value)) &&
      (p.state !== 'reorged' || leaving.value.has(poolKey(p.chainId, p.address))),
  ),
)

// A deep-linked pool that is not in the loaded feed is fetched on its own.
const selectedInFeed = computed(() => {
  const want = selParam.value?.toLowerCase()
  if (!want) return undefined
  const match = (p: Pool) => poolKey(p.chainId, p.address) === want
  return all.value.find(match) ?? favoriteRows.value.find(match) ?? searchRows.value.find(match)
})
const detailKey = computed(() => (selParam.value && !selectedInFeed.value && !feed.isLoading.value ? parsePoolKey(selParam.value) : null))
const detail = usePoolQuery(detailKey)

const selected = computed<Pool | null>(() => {
  if (selParam.value) return selectedInFeed.value ?? detail.data.value ?? null
  return (searching.value ? searchRows.value[0] : rows.value[0]) ?? null
})
const selectedKey = computed(() => (selected.value ? poolKey(selected.value.chainId, selected.value.address) : null))

function writeQuery(patch: LocationQueryRaw) {
  const next: LocationQueryRaw = { ...route.query, ...patch }
  for (const k of Object.keys(next)) if (next[k] === undefined || next[k] === 'all') delete next[k]
  void router.replace({ query: next })
}

function setChain(id: string) {
  addChain.value = ''
  writeQuery({ chain: id, sel: undefined })
}
function setRisk(id: RiskFilter) {
  writeQuery({ risk: id, sel: undefined })
}
function setDex(slug: string) {
  writeQuery({ dex: slug, sel: undefined })
}
function setColumns(f: ColumnFilters) {
  const patch: LocationQueryRaw = { sel: undefined }
  for (const k of COLUMN_KEYS) patch[k] = encodeRange(f[k])
  writeQuery(patch)
}
function select(p: Pool) {
  writeQuery({ sel: poolKey(p.chainId, p.address) })
}

function loadMore() {
  if (feed.hasNextPage.value && !feed.isFetchingNextPage.value) void feed.fetchNextPage()
}

// A narrow filter over a feed sorted by age can match little on the first page;
// keep pulling pages until there is something to show or the backlog ends.
let autoPages = 0
watch(
  () => [rows.value.length, feed.hasNextPage.value, feed.isFetchingNextPage.value, filterSignature.value] as const,
  ([count, hasNext, fetching], old) => {
    if (old && old[3] !== filterSignature.value) autoPages = 0
    // Column filters can be narrow (liquidity above $100K); look further back for them.
    const limit = columnsActive.value || dex.value !== ALL_DEXES ? 25 : 10
    if (!fetching && hasNext && count < 25 && autoPages < limit && feed.data.value) {
      autoPages++
      loadMore()
    }
  },
)


const feedError = computed(() => (feed.isError.value ? errorMessage(feed.error.value) : null))
const detailMissing = computed(() => !!selParam.value && !selected.value && detail.isError.value)
</script>

<template>
  <div class="content">
    <main class="main">
      <SearchBar v-model="searchText" :hint="searchHint" />
      <p v-if="addedNote" class="added-note" role="status" data-testid="add-pool-done">{{ addedNote }}</p>
      <template v-if="searching">
      <PoolTable
        variant="search"
        :rows="searchRows"
        :total="searchRows.length"
        :favorite-keys="starred"
        :selected-key="selectedKey"
        :filter-signature="`search:${term ?? ''}`"
        :loading="!!term && search.isPending.value"
        :error="searchError"
        :has-more="false"
        :loading-more="false"
        :empty-text="searchEmpty"
        @select="select"
        @toggle-favorite="toggleFavorite"
        @retry="search.refetch()"
      />
      <div v-if="canAddPool" class="add-pool card" data-testid="add-pool">
        <div class="add-row">
          <span class="add-text">Not listed here. Add it by address to trade it:</span>
          <select v-model="addChain" class="add-select" aria-label="Chain to add the pool on" data-testid="add-pool-chain">
            <option v-for="c in addChains" :key="c.id" :value="c.id">{{ c.name }}</option>
          </select>
          <button type="button" class="add-btn" :disabled="adding || !addChainId" data-testid="add-pool-submit" @click="addPool">
            {{ adding ? 'Reading the chain…' : 'Add pool' }}
          </button>
        </div>
        <p v-if="addError" class="add-err" role="status">{{ addError }}</p>
        <p v-else class="add-note">It is read from the chain, checked against its factory, and starred so its figures stay live.</p>
      </div>
      </template>
      <template v-else>
      <PoolTable
        v-if="favoriteRows.length"
        variant="favorites"
        :rows="favoriteRows"
        :total="favoriteRows.length"
        :favorite-keys="starred"
        :selected-key="selectedKey"
        filter-signature="favorites"
        :loading="false"
        :error="null"
        :has-more="false"
        :loading-more="false"
        @select="select"
        @toggle-favorite="toggleFavorite"
      />
      <FilterBar
        :chain="chain"
        :risk="risk"
        :dex="dex"
        :chains="chains.data.value"
        :columns="columns"
        @chain="setChain"
        @risk="setRisk"
        @dex="setDex"
        @columns="setColumns"
      />
      <PoolTable
        :rows="rows"
        :favorite-keys="starred"
        :total="all.length"
        :selected-key="selectedKey"
        :filter-signature="filterSignature"
        :loading="feed.isLoading.value"
        :error="feedError"
        :has-more="!!feed.hasNextPage.value"
        :loading-more="feed.isFetchingNextPage.value"
        @select="select"
        @toggle-favorite="toggleFavorite"
        @load-more="loadMore"
        @retry="feed.refetch()"
      />
      </template>
    </main>

    <aside class="aside" aria-label="Selected pool">
      <FundsCard />
      <template v-if="selected">
        <TradePanel :pool="selected" />
        <FraudCard :pool="selected" />
      </template>
      <section v-else class="card empty">
        <template v-if="detailMissing">That pool could not be found. It may have been reorged out.</template>
        <template v-else-if="feed.isLoading.value || detail.isLoading.value">Loading pool…</template>
        <template v-else>Select a pool to trade and see its fraud checks.</template>
      </section>
    </aside>
  </div>
</template>

<style scoped>
.content {
  display: flex;
  gap: 14px;
  align-items: flex-start;
  width: 100%;
  max-width: 1280px;
  margin: 0 auto;
  padding: 18px 24px 28px;
}
.main {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.aside {
  flex: 0 1 372px;
  min-width: 300px;
  position: sticky;
  top: var(--header-h);
  display: flex;
  flex-direction: column;
  gap: 14px;
  /* Taller than the viewport, a sticky column would hide its own bottom; let it scroll instead. */
  max-height: calc(100vh - var(--header-h));
  overflow-y: auto;
  overscroll-behavior: contain;
  padding-bottom: 14px;
  scrollbar-width: thin;
}
/* Cards keep their natural height; the column scrolls instead of squeezing them into each other. */
.aside > * {
  flex-shrink: 0;
}
.empty {
  padding: 22px 16px;
  font-family: var(--font-mono);
  font-size: 12px;
  color: var(--fg-muted);
  text-align: center;
}
@media (max-width: 1099px) {
  .content {
    flex-direction: column;
    align-items: stretch;
  }
  .aside {
    position: static;
    flex: none;
    min-width: 0;
    max-height: none;
    overflow: visible;
  }
}
@media (max-width: 819px) {
  .content {
    padding: 12px 16px 24px;
  }
}

.add-pool {
  padding: 12px 14px;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.add-row {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.add-text {
  font-family: var(--font-mono);
  font-size: 12px;
}
.add-select {
  font: inherit;
  font-size: 12px;
  padding: 5px 8px;
  border-radius: 8px;
  border: 1px solid var(--chip-2);
  background: var(--surface-2);
  color: inherit;
}
.add-btn {
  font-size: 12px;
  font-weight: 600;
  padding: 6px 12px;
  border-radius: 8px;
  border: 1px solid oklch(0.38 0.06 168);
  color: oklch(0.84 0.12 168);
  background: transparent;
  cursor: pointer;
}
.add-btn:disabled {
  opacity: 0.5;
  cursor: progress;
}
.add-note,
.add-err,
.added-note {
  margin: 0;
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--fg-muted);
  line-height: 1.5;
}
.add-err {
  color: oklch(0.82 0.12 25);
}
.added-note {
  padding: 8px 2px 0;
}
</style>