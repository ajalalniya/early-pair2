<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import TradePanel from '@/components/aside/TradePanel.vue'
import ChainTag from '@/components/ui/ChainTag.vue'
import Chip from '@/components/ui/Chip.vue'
import RiskChip from '@/components/ui/RiskChip.vue'
import ExitModal from '@/components/portfolio/ExitModal.vue'
import LockedCard from '@/components/ui/LockedCard.vue'
import { ApiError, errorMessage } from '@/lib/api'
import { chainMeta, parsePoolKey, poolKey } from '@/lib/chains'
import { DASH, moneyFine, pct, price, signedMoneyFine, units } from '@/lib/format'
import { useQueryClient } from '@tanstack/vue-query'
import { useTxExecutor } from '@/composables/useTxExecutor'
import { useChainsQuery, useHideHolding, useHoldingsQuery, usePoolQuery, usePositionsQuery } from '@/queries'
import { useToasts } from '@/stores/toasts'
import { useWallet } from '@/stores/wallet'
import type { Holding, Position } from '@/types/api'

const wallet = useWallet()
const route = useRoute()
const router = useRouter()
const holdings = useHoldingsQuery()
const positions = usePositionsQuery()
const hideHolding = useHideHolding()

// Removing a token only takes it off this list; the wallet still holds it, and
// a later trade in it lists it again. The confirmation is inline in the row:
// a native confirm() can be suppressed by the browser, and then it answers
// "cancel" without showing anything.
const rowId = (h: Holding) => `${h.chainId}:${h.token.address.toLowerCase()}`
const confirmingRemove = ref<string | null>(null)

function removeHolding(h: Holding) {
  confirmingRemove.value = null
  if (holdingKey(h) === selParam.value) void router.replace({ query: { ...route.query, sel: undefined } })
  hideHolding.mutate(h)
}

// ------------------------------------------------------------ approve ahead
// Selling a token needs its approvals first — two for a Uniswap v4 pool, one
// otherwise. Signing them ahead, while nothing is urgent, makes the sell itself
// a single transaction when the moment comes. The backend reports which
// holdings are already approved (`sellReady`), so the button only shows where
// it would do something.
const approver = useTxExecutor()
const chainsInfo = useChainsQuery()
const toasts = useToasts()
const qc = useQueryClient()
const approvingRow = ref<string | null>(null)

async function approveHolding(h: Holding) {
  if (!h.pool || approvingRow.value) return
  approvingRow.value = rowId(h)
  const info = chainsInfo.data.value?.find((c) => c.id === h.chainId)
  const ok = await approver.approveAhead({ chainId: h.chainId, pool: h.pool, side: 'sell' }, chainMeta(h.chainId, info).numericId)
  approvingRow.value = null
  if (ok) {
    toasts.push(`${h.token.symbol} approved — selling it is now one transaction.`, 'accent')
    void qc.invalidateQueries({ queryKey: ['portfolio', 'holdings'] })
  } else if (approver.error.value) {
    toasts.push(`Could not approve ${h.token.symbol}: ${approver.error.value}`, 'danger')
  }
}

const sum = (xs: (number | null)[]) => (xs.length && xs.every((x) => x !== null) ? xs.reduce<number>((a, b) => a + (b as number), 0) : null)

const tokenTotal = computed(() => (holdings.data.value ? sum(holdings.data.value.map((h) => h.valueUsd)) : null))
const lpTotal = computed(() => (positions.data.value ? sum(positions.data.value.map((p) => p.valueUsd)) : null))
const feesTotal = computed(() => (positions.data.value ? sum(positions.data.value.map((p) => p.feesUsd)) : null))
const netWorth = computed(() => (tokenTotal.value !== null && lpTotal.value !== null ? tokenTotal.value + lpTotal.value : null))
const pnl = computed(() => (holdings.data.value ? sum(holdings.data.value.map((h) => h.pnlUsd)) : null))
const pnlPct = computed(() => {
  if (pnl.value === null || tokenTotal.value === null) return null
  const cost = tokenTotal.value - pnl.value
  return cost > 0 ? (pnl.value / cost) * 100 : null
})

function reason(err: unknown): string {
  if (err instanceof ApiError && err.notImplemented) return `Not available yet: ${err.message}`
  return errorMessage(err)
}

// ------------------------------------------------------------ trade panel
// A holding opens the same summary card and trade panel as the pools page,
// beside the table, to sell or buy more without leaving the portfolio. The
// selection lives in the URL, and outlives the holding: selling it all keeps
// the panel open on its receipt.
const holdingKey = (h: Holding) => (h.pool ? poolKey(h.chainId, h.pool) : null)
const selParam = computed(() => (typeof route.query.sel === 'string' ? route.query.sel.toLowerCase() : null))
const selectedKey = computed(() => selParam.value ?? holdings.data.value?.map(holdingKey).find((k) => k !== null) ?? null)
const selectedPool = usePoolQuery(computed(() => (selectedKey.value ? parsePoolKey(selectedKey.value) : null)))
const selectedHolding = computed(() => holdings.data.value?.find((h) => holdingKey(h) === selectedKey.value) ?? null)

const sideRequest = ref<{ side: 'buy' | 'sell'; n: number }>({ side: 'sell', n: 0 })

function openTrade(h: Holding, side?: 'buy' | 'sell') {
  const key = holdingKey(h)
  if (!key) return
  if (side) sideRequest.value = { side, n: sideRequest.value.n + 1 }
  void router.replace({ query: { ...route.query, sel: key } })
}

function openPool(p: Position, tab: 'lp') {
  void router.push({ path: '/pools', query: { sel: poolKey(p.chainId, p.pool), tab } })
}

const exiting = ref<Position | null>(null)
</script>

<template>
  <div class="page">
    <LockedCard
      v-if="!wallet.connected"
      title="Connect your wallet"
      body="Your holdings, LP positions and claimable fees appear here once a wallet is connected. Read-only — nothing is signed."
      action="Connect Wallet"
      @action="wallet.openModal()"
    />
    <LockedCard
      v-else-if="!wallet.signedIn"
      title="Sign in to view your portfolio"
      :body="wallet.signInError ?? 'Sign a one-time message to prove this address is yours. It authorises nothing else and costs no gas.'"
      :action="wallet.signingIn ? 'Waiting for signature…' : 'Sign in'"
      :busy="wallet.signingIn"
      @action="wallet.signIn()"
    />

    <div v-else class="layout">
    <div class="portfolio">
      <div class="stats">
        <div class="stat">
          <div class="caps">NET WORTH</div>
          <div class="stat-v mono">{{ moneyFine(netWorth) }}</div>
        </div>
        <div class="stat">
          <div class="caps">UNREALIZED P&amp;L</div>
          <div class="stat-row mono" :data-tone="pnl === null ? '' : pnl >= 0 ? 'up' : 'down'">
            <span class="stat-v">{{ signedMoneyFine(pnl) }}</span>
            <span v-if="pnlPct !== null" class="stat-pct">{{ pct(pnlPct) }}</span>
          </div>
        </div>
        <div class="stat">
          <div class="caps">IN LP POSITIONS</div>
          <div class="stat-v mono">{{ moneyFine(lpTotal) }}</div>
        </div>
        <div class="stat">
          <div class="caps">FEES EARNED</div>
          <div class="stat-v mono fees">{{ signedMoneyFine(feesTotal) }}</div>
        </div>
      </div>

      <section class="section">
        <div class="section-head">
          <h2>Tokens bought</h2>
          <span class="mono count">{{ holdings.data.value ? `${holdings.data.value.length} tokens held · ${moneyFine(tokenTotal)}` : DASH }}</span>
        </div>
        <div class="card holdings">
          <div class="h-table">
            <div class="h-grid h-head">
              <div>TOKEN</div>
              <div class="r">AMOUNT</div>
              <div class="r col-avg">AVG ENTRY</div>
              <div class="r col-price">PRICE</div>
              <div class="r">VALUE</div>
              <div class="r">P&amp;L</div>
              <div class="c">RISK</div>
              <div />
            </div>
            <div v-if="holdings.isLoading.value" class="state">Loading holdings…</div>
            <div v-else-if="holdings.isError.value" class="state">{{ reason(holdings.error.value) }}</div>
            <div v-else-if="!holdings.data.value?.length" class="state">No tokens held. Tokens you buy here appear once the trade confirms.</div>
            <div
              v-for="h in holdings.data.value"
              :key="`${h.chainId}:${h.token.address}`"
              class="h-grid h-row"
              :class="{ selected: holdingKey(h) === selectedKey, clickable: !!h.pool }"
              :data-testid="`holding-${h.chainId}:${h.token.address.toLowerCase()}`"
              @click="openTrade(h)"
            >
              <div class="token">
                <ChainTag :chain-id="h.chainId" />
                <div class="min0">
                  <span class="mono sym">{{ h.token.symbol }}</span>
                  <div v-if="h.note" class="mono note">{{ h.note }}</div>
                </div>
              </div>
              <div class="num"><span class="lbl">AMOUNT</span>{{ units(h.amount, h.token.decimals) }}</div>
              <div class="num dim col-avg"><span class="lbl">AVG ENTRY</span>{{ h.avgEntryUsd !== null ? '$' + price(h.avgEntryUsd) : DASH }}</div>
              <div class="num col-price"><span class="lbl">PRICE</span>{{ h.priceUsd !== null ? '$' + price(h.priceUsd) : DASH }}</div>
              <div class="num strong"><span class="lbl">VALUE</span>{{ moneyFine(h.valueUsd) }}</div>
              <div class="num pnl" :data-tone="h.pnlPct === null ? '' : h.pnlPct >= 0 ? 'up' : 'down'" :title="h.note">
                <span class="lbl">P&amp;L</span>
                <span><span class="pnl-pct">{{ pct(h.pnlPct) }}</span><span class="pnl-abs">{{ signedMoneyFine(h.pnlUsd) }}</span></span>
              </div>
              <div class="c risk"><RiskChip :risk="h.risk" :show-score="false" /></div>
              <div v-if="confirmingRemove === rowId(h)" class="r actions" data-testid="holding-remove-confirm" @click.stop>
                <button
                  type="button"
                  class="act-btn sell"
                  data-testid="holding-remove-yes"
                  :title="`Remove ${h.token.symbol} from this list. The tokens stay in your wallet; trading it here lists it again.`"
                  @click.stop="removeHolding(h)"
                >
                  Remove
                </button>
                <button type="button" class="act-btn remove" data-testid="holding-remove-no" @click.stop="confirmingRemove = null">Cancel</button>
              </div>
              <div v-else class="r actions">
                <template v-if="h.pool">
                  <button type="button" class="act-btn buy" data-testid="holding-buy" @click.stop="openTrade(h, 'buy')">Buy more</button>
                  <button type="button" class="act-btn sell" data-testid="holding-sell" @click.stop="openTrade(h, 'sell')">Sell</button>
                  <button
                    v-if="h.sellReady === false"
                    type="button"
                    class="act-btn approve"
                    data-testid="holding-approve"
                    :disabled="approvingRow !== null"
                    :title="`Approve ${h.token.symbol} now, so selling it later is a single transaction`"
                    @click.stop="approveHolding(h)"
                  >
                    {{ approvingRow === rowId(h) ? 'Approving…' : 'Approve' }}
                  </button>
                  <span
                    v-else-if="h.sellReady === true"
                    class="ready-chip"
                    data-testid="holding-ready"
                    :title="`${h.token.symbol} is approved: selling it is one transaction`"
                  >READY</span>
                </template>
                <span v-else class="act-btn sell disabled" title="No indexed pool to trade in">Sell</span>
                <button
                  type="button"
                  class="act-btn remove"
                  data-testid="holding-remove"
                  :title="`Remove ${h.token.symbol} from this list (the tokens stay in your wallet)`"
                  :aria-label="`Remove ${h.token.symbol} from this list`"
                  @click.stop="confirmingRemove = rowId(h)"
                >
                  <svg viewBox="0 0 16 16" width="13" height="13" aria-hidden="true">
                    <path d="M3 4.5h10M6.5 4.5V3h3v1.5M5 4.5l.6 8.5h4.8l.6-8.5" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" />
                  </svg>
                </button>
              </div>
            </div>
          </div>
        </div>
      </section>

      <section class="section">
        <div class="section-head">
          <h2>Liquidity positions</h2>
          <span class="mono count">{{ positions.data.value ? `${positions.data.value.length} active positions · ${moneyFine(lpTotal)}` : DASH }}</span>
        </div>
        <div v-if="positions.isLoading.value" class="card state">Loading positions…</div>
        <div v-else-if="positions.isError.value" class="card state">{{ reason(positions.error.value) }}</div>
        <div v-else-if="!positions.data.value?.length" class="card state">No liquidity positions.</div>
        <div v-else class="lp-grid">
          <article v-for="p in positions.data.value" :key="p.id" class="card lp">
            <div class="lp-top">
              <div class="token">
                <ChainTag :chain-id="p.chainId" />
                <div class="min0">
                  <div class="mono lp-pair">{{ p.pair }}</div>
                  <div class="mono lp-dex">{{ p.dex }}</div>
                </div>
              </div>
              <Chip v-if="p.inRange !== null" :tone="p.inRange ? 'accent' : 'warn'">{{ p.inRange ? 'In range' : 'Out of range' }}</Chip>
            </div>
            <div class="lp-tiles">
              <div class="tile">
                <div class="t-label">POSITION</div>
                <div class="t-value">{{ moneyFine(p.valueUsd) }}</div>
              </div>
              <div class="tile">
                <div class="t-label">FEES EARNED</div>
                <div class="t-value fees">{{ signedMoneyFine(p.feesUsd) }}</div>
              </div>
            </div>
            <div class="kv">
              <div><span>Pool share</span><span>{{ p.sharePct !== null ? p.sharePct.toFixed(2) + ' %' : DASH }}</span></div>
              <div><span>Est. APR</span><span>{{ p.aprPct !== null ? p.aprPct.toFixed(1) + ' %' : DASH }}</span></div>
            </div>
            <div class="lp-actions">
              <button class="btn btn-accent grow" type="button" @click="openPool(p, 'lp')">Add</button>
              <button class="btn grow" type="button" @click="openPool(p, 'lp')">Remove</button>
            </div>
            <button class="btn btn-danger-outline" type="button" @click="exiting = p">Remove + sell → {{ p.quoteSymbol }}</button>
          </article>
        </div>
      </section>
    </div>

    <aside class="aside" aria-label="Trade selected holding">
      <TradePanel
        v-if="selectedPool.data.value"
        :pool="selectedPool.data.value"
        :side-request="sideRequest"
        :held="selectedHolding?.amount ?? null"
      />
      <section v-else class="card empty">
        <template v-if="selectedPool.isLoading.value">Loading pool…</template>
        <template v-else-if="holdings.data.value?.length">Select a token to sell it or buy more.</template>
        <template v-else>Tokens you buy appear here, ready to sell or buy more.</template>
      </section>
    </aside>
    </div>

    <ExitModal v-if="exiting" :position="exiting" @close="exiting = null" />
  </div>
</template>

<style scoped>
.page {
  width: 100%;
  max-width: 1280px;
  margin: 0 auto;
  padding: 18px 24px 40px;
}
.layout {
  display: flex;
  gap: 14px;
  align-items: flex-start;
}
.portfolio {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 22px;
}
.aside {
  flex: 0 1 372px;
  min-width: 300px;
  position: sticky;
  top: var(--header-h);
  display: flex;
  flex-direction: column;
  gap: 14px;
  max-height: calc(100vh - var(--header-h));
  overflow-y: auto;
  overscroll-behavior: contain;
  padding-bottom: 14px;
  scrollbar-width: thin;
}
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
.stats {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(170px, 1fr));
  gap: 10px;
}
.stat {
  border: 1px solid var(--chip-2);
  border-radius: 12px;
  padding: 13px 15px;
  background: var(--surface-2);
}
.stat-v {
  font-size: 26px;
  font-weight: 600;
  margin-top: 6px;
}
.stat-row {
  display: flex;
  align-items: baseline;
  gap: 8px;
  margin-top: 6px;
}
.stat-row .stat-v {
  margin-top: 0;
}
.stat-pct {
  font-size: 13px;
}
[data-tone='up'] {
  color: oklch(0.84 0.15 168);
}
[data-tone='down'] {
  color: oklch(0.74 0.16 25);
}
.fees {
  color: oklch(0.84 0.14 168);
}
.section {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.section-head {
  display: flex;
  align-items: baseline;
  gap: 12px;
  flex-wrap: wrap;
}
h2 {
  margin: 0;
  font-size: 17px;
  font-weight: 600;
  letter-spacing: -0.01em;
}
.count {
  font-size: 12px;
  color: var(--fg-muted);
}
.holdings {
  /* Scroll rather than clip: with long amounts a row can be wider than the
     card, and clipping silently hid the right-most buttons (delete first). */
  overflow-x: auto;
  container: holdings / inline-size;
}
/* One set of column tracks for the header and every row. As separate grids
   each sized its columns from its own content, so the header drifted out of
   line with the rows below it. Numbers never shrink below their widest value;
   the actions take their buttons' width on one line when there is room, and
   wrap them (never below 168px) when there is not. */
.h-table {
  display: grid;
  grid-template-columns: minmax(110px, 1.3fr) repeat(5, minmax(max-content, 1fr)) 72px minmax(168px, max-content);
  column-gap: 10px;
}
.h-table > .state {
  grid-column: 1 / -1;
}
.h-grid {
  grid-column: 1 / -1;
  display: grid;
  grid-template-columns: subgrid;
  align-items: center;
  padding: 13px 16px;
}
/* A narrow card cannot hold every column and the actions at once. Average
   entry and price give way first — value and P&L carry the same news — so the
   row's buttons, the delete button last among them, stay on screen. */
@media (min-width: 820px) {
  @container holdings (max-width: 820px) {
    .h-table {
      grid-template-columns: minmax(96px, 1.2fr) repeat(3, minmax(max-content, 1fr)) 72px minmax(168px, max-content);
    }
    .col-avg,
    .col-price {
      display: none;
    }
  }
}
.h-head {
  padding: 11px 16px;
  border-bottom: 1px solid var(--chip-2);
  font-family: var(--font-mono);
  font-size: 10.5px;
  letter-spacing: 0.07em;
  color: var(--fg-muted);
}
.h-row {
  border-bottom: 1px solid var(--border-faint);
}
.h-row:hover {
  background: oklch(0.215 0.013 265);
}
.h-row.clickable {
  cursor: pointer;
}
.h-row.selected {
  background: var(--row-hover);
}
.r {
  text-align: right;
}
.c {
  text-align: center;
  display: flex;
  justify-content: center;
}
.token {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
}
.sym {
  font-size: 13px;
  font-weight: 600;
}
.note {
  margin-top: 2px;
  font-size: 10.5px;
  line-height: 1.35;
  color: var(--fg-muted);
}
.num {
  text-align: right;
  font-family: var(--font-mono);
  font-size: 13px;
}
.dim {
  color: var(--fg-3);
}
.strong {
  font-weight: 500;
}
.pnl-pct {
  display: block;
  font-weight: 600;
}
.pnl-abs {
  display: block;
  font-size: 11px;
  opacity: 0.85;
}
.lbl {
  display: none;
}
.actions {
  display: flex;
  flex-wrap: wrap;
  justify-content: flex-end;
  align-items: center;
  gap: 6px;
}
.act-btn {
  font-size: 11.5px;
  font-weight: 600;
  padding: 5px 9px;
  border-radius: 7px;
  background: transparent;
  white-space: nowrap;
  cursor: pointer;
}
.act-btn.buy {
  border: 1px solid oklch(0.38 0.06 168);
  color: oklch(0.84 0.12 168);
}
.act-btn.sell {
  border: 1px solid oklch(0.38 0.06 25);
  color: oklch(0.82 0.12 25);
}
.act-btn.approve {
  border: 1px solid oklch(0.4 0.07 250);
  color: oklch(0.84 0.1 250);
}
.act-btn.approve:disabled {
  opacity: 0.55;
  cursor: progress;
}
.ready-chip {
  align-self: center;
  font-family: var(--font-mono);
  font-size: 9.5px;
  font-weight: 600;
  letter-spacing: 0.05em;
  padding: 2px 6px;
  border-radius: 4px;
  color: oklch(0.84 0.12 168);
  border: 1px solid oklch(0.38 0.06 168);
}
.act-btn.remove {
  display: inline-flex;
  align-items: center;
  padding: 5px 6px;
  border: 1px solid var(--chip-2);
  color: var(--fg-muted);
}
.act-btn.remove:hover {
  color: oklch(0.82 0.12 25);
  border-color: oklch(0.38 0.06 25);
}
.act-btn.disabled {
  opacity: 0.45;
  cursor: not-allowed;
}
.state {
  padding: 26px 16px;
  font-family: var(--font-mono);
  font-size: 12px;
  color: var(--fg-muted);
  text-align: center;
  line-height: 1.6;
}
.lp-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(290px, 1fr));
  gap: 12px;
}
.lp {
  padding: 15px;
  display: flex;
  flex-direction: column;
  gap: 13px;
}
.lp-top {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 10px;
}
.min0 {
  min-width: 0;
}
.lp-pair {
  font-size: 14px;
  font-weight: 600;
}
.lp-dex {
  font-size: 11px;
  color: var(--fg-muted);
  margin-top: 2px;
}
.lp-tiles {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 9px;
}
.t-label {
  font-size: 10.5px;
  font-family: var(--font-mono);
  color: var(--fg-muted);
  letter-spacing: 0.05em;
}
.t-value {
  font-size: 16px;
  font-weight: 600;
  margin-top: 4px;
  font-family: var(--font-mono);
}
.lp-actions {
  display: flex;
  gap: 8px;
}
.grow {
  flex: 1;
}

@media (max-width: 1099px) {
  .layout {
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
  .h-table {
    display: block;
  }
  .page {
    padding: 12px 16px 32px;
  }
  .h-head {
    display: none;
  }
  .h-grid.h-row {
    grid-template-columns: 1fr 1fr;
    gap: 6px 14px;
  }
  .h-row .token {
    grid-column: 1 / -1;
  }
  .h-row .num {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    gap: 6px;
    font-size: 12.5px;
  }
  .lbl {
    display: inline;
    font-size: 10px;
    letter-spacing: 0.05em;
    color: var(--fg-muted);
  }
  .pnl-pct,
  .pnl-abs {
    display: inline;
    margin-left: 6px;
  }
  .h-row .risk {
    justify-content: flex-start;
  }
}
</style>
