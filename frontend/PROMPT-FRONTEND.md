# EarlyPair — Frontend Build Prompt (Vue 3 + TypeScript)

You are building the production frontend for **EarlyPair**, a multi-chain new-pair
discovery and trading terminal. A complete interactive design prototype exists at
`EarlyPair Standalone.html` (a Claude Design canvas — open it in a browser to see the
target). Your job is to rebuild it as a real Vue application wired to the EarlyPair Go
backend. **The prototype is the visual and behavioural spec: match it.**

---

## 1. Product in one paragraph

EarlyPair watches liquidity-pool factory contracts across Ethereum, Base, BSC, Arbitrum,
Robinhood Chain, Arc, Solana and SUI, and surfaces brand-new pairs within seconds of
deployment. Each pair gets a 0–100 fraud score from on-chain checks. The user can buy,
sell, "zap" (buy + provide liquidity in one transaction), manage concentrated-liquidity
ranges, and exit (remove LP + sell into the quote token in one transaction). It is
non-custodial — the backend builds unsigned transactions, the wallet signs them. There is
also a watchlist: name a token and get alerted when a new pair for it is deployed
anywhere, or register an event signature / topic0 and get every matching log.

---

## 2. Stack

- **Vue 3.5+**, `<script setup lang="ts">`, Composition API only. No Options API.
- **Vite 6** + `vue-tsc` strict mode. Path alias `@/ -> src/`.
- **Pinia** for cross-view state (wallet, settings, stream status).
- **vue-router 4** — routes `/pools` (default), `/portfolio`, `/watchlist`. Filters and
  the selected pool live in the query string (`/pools?chain=base&risk=safe&sel=0xabc…`)
  so every view is linkable and restorable.
- **@tanstack/vue-query** for every REST read. Live WebSocket messages are written into
  the query cache via `queryClient.setQueryData`, never into a parallel store — one
  source of truth per entity.
- **viem + wagmi** (EVM), **@solana/wallet-adapter** (Solana), **@mysten/dapp-kit** (SUI),
  WalletConnect via **@reown/appkit**.
- **Styling: plain CSS with custom properties** in `src/styles/tokens.css` plus scoped
  `<style>` per SFC. Do **not** add Tailwind — the design is built on an `oklch()` token
  scale that maps 1:1 to CSS variables.
- **Vitest** + `@vue/test-utils` for units; **Playwright** for three flows: connect
  wallet, risk-gated buy, zap in.

---

## 3. Design system — copy these exact values

Fonts (already referenced in the prototype, load from Google Fonts):
- `'Space Grotesk', sans-serif` — UI text, headings, buttons.
- `'JetBrains Mono', monospace` — **every number, address, hash, URL, ticker and caps
  label**. Mono is the dominant face in this design; when in doubt on a numeric cell,
  use mono.

Colour tokens (`oklch`, **dark theme only** — do not build a light theme):

```css
:root {
  /* surfaces, darkest to lightest */
  --bg:            oklch(0.16 0.012 265);
  --surface:       oklch(0.185 0.012 265);
  --surface-2:     oklch(0.19 0.012 265);
  --row-hover:     oklch(0.225 0.014 265);
  --row-selected:  oklch(0.235 0.012 265);
  --chip:          oklch(0.25 0.015 265);
  --chip-2:        oklch(0.26 0.015 265);
  --chip-active:   oklch(0.27 0.02 265);
  /* borders */
  --border:        oklch(0.27 0.015 265);
  --border-strong: oklch(0.30 0.015 265);
  --border-active: oklch(0.45 0.02 265);
  /* text */
  --fg:            oklch(0.96 0.005 265);
  --fg-2:          oklch(0.88 0.01 265);
  --fg-3:          oklch(0.70 0.01 265);
  --fg-muted:      oklch(0.64 0.01 265);
  /* semantic */
  --accent:        oklch(0.8 0.13 168);   /* mint: primary action, safe, gains */
  --on-accent:     oklch(0.19 0.04 265);
  --warn:          oklch(0.82 0.15 85);   /* amber: watch, out-of-range        */
  --danger:        oklch(0.68 0.17 25);   /* red: danger, sell, losses         */
  --info:          oklch(0.78 0.15 265);  /* violet: concentrated, WSS badge   */
}
```

Risk bands (used everywhere a score appears):

| score | label    | fg                     | bg                     |
|-------|----------|------------------------|------------------------|
| < 25  | `SAFE`   | `oklch(0.86 0.14 168)` | `oklch(0.30 0.07 168)` |
| 25–59 | `WATCH`  | `oklch(0.88 0.14 85)`  | `oklch(0.33 0.07 85)`  |
| >= 60 | `DANGER` | `oklch(0.88 0.13 25)`  | `oklch(0.34 0.11 25)`  |

Chain dot colours: ETH `oklch(0.72 0.12 285)`, BASE `oklch(0.68 0.17 260)`,
BSC `oklch(0.82 0.15 95)`, ARB `oklch(0.7 0.13 235)`, RBN `oklch(0.8 0.16 145)`,
SOL `oklch(0.72 0.16 310)`, SUI `oklch(0.78 0.1 220)`.

Shape language: 8–12px radii on cards, 6–8px on chips and buttons, 1px borders, no
shadows, generous use of 1px dividers instead of elevation.

Accent is user-selectable in settings — the prototype offers mint (default),
`oklch(0.78 0.15 265)`, `oklch(0.82 0.15 85)`, `oklch(0.75 0.16 320)`. Drive everything
from `--accent` so one variable swap restyles the app.

---

## 4. Layout

```
┌ header (sticky, ~78px, border-bottom) ──────────────────────────────────────┐
│ logo + wordmark │ nav: New Pools · Portfolio · Watchlist │ ⟨spacer⟩         │
│                               │ RPC health pill (click -> dropdown panel)   │
│                               │ wallet button (Connect Wallet / 0x7a4f…2Be1)│
└─────────────────────────────────────────────────────────────────────────────┘
  content: max-width 1280px, centred, display:flex, gap 14px
  ┌ main (flex:1, min-width:0) ────────────┐ ┌ aside ─────────────────────────┐
  │  filter bar: CHAIN chips | RISK chips  │ │ flex: 0 1 372px;               │
  │  pool table (grid, see below)          │ │ min-width: 300px;              │
  │  "N of M pools shown" footer           │ │ position: sticky; top: 78px;   │
  └────────────────────────────────────────┘ │ gap: 14px                      │
                                             │  · Available funds card        │
                                             │  · Trade panel (tabbed)        │
                                             │  · Fraud detection card        │
                                             └────────────────────────────────┘
```

Grid templates lifted from the design — use them verbatim:

- Pool table: `minmax(165px, 2fr) 62px 1fr 1fr 1.1fr 1fr 92px 116px`
  → POOL | AGE | LIQUIDITY | VOL 1H | BUYERS / SELLERS | PRICE 1H | RISK | Trade ↗
- Portfolio holdings: `minmax(150px, 1.6fr) 1.1fr 1fr 1fr 1fr 1.1fr 88px 74px`
  → TOKEN | AMOUNT | AVG ENTRY | PRICE | VALUE | P&L | RISK | Sell
- LP position cards: `repeat(auto-fill, minmax(290px, 1fr))`
- Funds by chain: `repeat(auto-fit, minmax(184px, 1fr))`
- Portfolio stat tiles: `repeat(auto-fit, minmax(170px, 1fr))`

**Responsive:** below 1100px the aside unsticks and moves under main. Below 820px both
tables become stacked cards (pair + chain on line 1, metrics as a two-column label/value
grid, risk chip and Trade button on the last line). Header nav collapses to a sheet. The
page body must never scroll horizontally.

---

## 5. Views and components

### 5.1 Header

- `<AppHeader>` — logo mark (mint rounded square + play triangle), wordmark "EarlyPair".
- `<NavTabs>` — New Pools / Portfolio / Watchlist. Active: bg `--surface-2`, fg `--fg`,
  weight 500.
- `<BlockPulse>` — a mono `BLOCK +Ns` label counting seconds since the last head event on
  the stream, reset by each `head` frame. Amber past 3× the chain's expected block time,
  red when the stream is down.
- `<RpcHealthPill>` — `"{healthy}/{total} RPCs HEALTHY"` + `"p50 {n} ms"` with a status
  dot (red if any endpoint is down, amber if any degraded, else accent). Click toggles
  `<RpcHealthPanel>`, one row per endpoint:
  chain tag + name · `WSS`/`HTTP` transport chip (WSS tinted with `--info`) · masked URL ·
  status chip `HEALTHY`/`DEGRADED`/`UNREACHABLE` · live latency in ms with a bar
  (`width = min(100, ms / 10)%`) · `"{n} subs · up {uptime}"` for sockets or
  `"polling {n} ms"` for HTTP · reconnect count (amber past 3) · head block number and lag
  (`tip` / `1 block behind` / `N blocks behind`) · an action button
  (`Resubscribe` / `Ping` / `Failover`) that calls the backend admin endpoint.
  Footer: `"{live}/{total} sockets live · {n} subscriptions · auto-resubscribe on socket
  close, HTTP fallback after 2 drops"` and an **Add endpoint** button (chain, transport,
  URL) that persists a user-supplied RPC.
- `<WalletButton>` -> `<WalletModal>`.

### 5.2 Wallet modal — three steps, exactly as prototyped

1. **choose** — connectors: MetaMask (`MM`, "Browser extension · EVM", `DETECTED`),
   Phantom (`PH`, "Solana · EVM", `DETECTED`), Rabby (`RB`, "Pre-sign risk simulation",
   `INSTALL`), WalletConnect (`WC`, "Scan with a mobile wallet", `QR`), Coinbase Wallet
   (`CB`, "Smart wallet · passkey", `INSTALL`). Badges come from real EIP-6963 provider
   discovery — `DETECTED` when the provider announced itself, `INSTALL` otherwise.
   Footer: *"EarlyPair never takes custody. Signing only proves address ownership —
   trades are approved per transaction."*
2. **pending** — "Approve in {wallet}", *"Waiting for the signature request. Nothing is
   submitted on-chain."*, Cancel (aborts the connector request, returns to step 1).
3. **done** — address `0x7a4f…2Be1`, `"{connector} · N chains"`, a `LIVE` chip, three
   balance tiles (Ethereum / Solana / Stables), **Start trading** and **Disconnect**.

Auth: after connect, `personal_sign` a backend nonce (SIWE) to obtain a session JWT for
portfolio and watchlist endpoints. Never request a private key or seed phrase.

### 5.3 New Pools view

- `<ChainFilter>` — All + 7 chains with colour dots. `<RiskFilter>` — All / Safe / Watch /
  Danger. Chip rows; changing either resets selection to the first row and writes to the
  query string.
- `<PoolTable>` — virtualised (`@tanstack/vue-virtual`); the live feed is unbounded.
  - Pair cell: mono pair name + chain tag chip with dot; dex name beneath in `--fg-muted`.
  - Age: mint (`oklch(0.86 0.12 168)`) when <= 10 minutes old, else `--fg-3`. Recompute
    from `createdAt` on a 1s ticker — never render a server-formatted age string.
  - Liquidity / volume via the money rule in §6.
  - Buyers / sellers as `{buyers} / {sellers}`; the seller figure turns
    `oklch(0.74 0.16 25)` when `buyers / sellers > 8` — the honeypot tell.
  - Price 1h: signed percentage, mint up / red down.
  - Risk chip `"{LABEL} {score}"` in band colours.
  - `Trade ↗` opens the pair on DexScreener in a new tab; clicking elsewhere on the row
    selects it into the aside.
  - **New rows arrive over the WebSocket:** insert at top with a 400ms highlight fade
    (respect `prefers-reduced-motion`). If the user has scrolled, do not jump — show a
    floating "N new pairs ↑" pill that scrolls to top on click.
  - Pools seen at 0 confirmations get a pulsing dot and an "unconfirmed" tooltip, cleared
    by the backend's `confirmed` update. On a `reorged` frame, fade the row out and toast
    once.
- Footer: `"{n} of {m} pools shown"`.

### 5.4 Aside — Available funds

`AVAILABLE FUNDS` card: total, `"Native + stables across N chains"` subtitle, a
`Stables {amount}` line, then one mini-card per chain (tag + dot + name + chain total,
then each asset as `sym / amount / usd`; USDC and USDT render in `--fg-3` to
de-emphasise them against native assets). Disconnected: every value is `—` and the
subtitle becomes "Connect a wallet to see balances".

### 5.5 Aside — Trade panel

Header: pair, `"{chain} · {dex} · {age} old"`, risk chip with score. Metric tiles:
LIQUIDITY, VOL 1H, UNIQUE BUYERS 1H, UNIQUE SELLERS 1H, plus a "Buyer : seller addresses"
ratio line (red above 8:1, `∞ : 1` when sellers is 0).

Three tabs — **Buy / Sell**, **Buy + LP**, **Liquidity**:

**Buy / Sell** — Buy/Sell segmented control (buy mint-tinted, sell red-tinted), large
mono amount input, "You receive" readout, then Rate / Price impact / Slippage rows
(slippage default 1.0 %, editable). Primary button label is state-driven, in priority
order: `"Blocked — high risk pool"` (risk gate) -> `"Connect wallet to trade"` ->
`"Buy {TOKEN}"` / `"Sell {TOKEN}"`.

**Buy + LP (zap)** — amount input, a **SPLIT** slider labelled
`"{n}% swap / {100-n}% paired"` with ends annotated *More LP depth* ←→ *More token
exposure*. Below, the numbered legs from the backend route plan:
1 `Swap {QUOTE} → {TOKEN}` · 2 `Deposit pair` · 3 `Mint LP position`. Then Fee tier /
est. APR, Pool share after, Swap impact + LP slip, Est. gas (bundled). Caption:
*"One transaction: swap half, then deposit both sides. Reverts entirely if any leg
fails."* Button cycles `Buy + add liquidity` -> `Confirm in wallet…` -> `Zapped`, then a
receipt line `"Bought {n} {TOKEN} and minted LP · 1 tx"` linking to the explorer.

**Liquidity** — Add / Remove sub-tabs. The panel adapts to the AMM model the backend
reports as `clmm` | `v2` | `stable`:

- `clmm` -> badge `CONCENTRATED` (violet). Fee-tier chips (0.01 / 0.05 / 0.30 / 1.00 %),
  range presets ±2 % / ±5 % / ±15 % / Full range / Custom, MIN PRICE and MAX PRICE
  steppers (▲▼ = ±1 %), and a **draggable range band** over a price axis with the current
  price marked. Derived readouts: Current price, Range width (`±X%`), Capital efficiency
  (`×N`), Status (`In range now` / `Out of range` / `Always in range` / `Invalid range`).
  Amber warning when out of range (*"Price is outside your range — this position earns no
  fees until it comes back"*) or when min >= max (*"Min price must be below max price."*),
  in which case the button reads `Fix price range` and is disabled. Note: *"Liquidity is
  only active while price stays inside your range — narrower earns more fees but goes
  idle sooner."*
- `v2` -> badge `FULL RANGE · 50 / 50`, no range UI. Note: *"Deposits are split 50 / 50
  and spread across the entire price curve — no range to manage."*
- `stable` -> badge `STABLESWAP`. Note: *"Deposits enter at the pool's current ratio
  across the whole curve; an imbalance fee applies if you deposit one-sided."*

The range drag must use pointer events (mouse + touch), clamp min < max, and freeze the
axis viewport while dragging so the handle does not chase the cursor. Selecting a preset
clears any custom bounds; editing a bound switches the preset chip to `Custom`.

### 5.6 Aside — Fraud detection

`FRAUD DETECTION` card: score `{n} / 100`, a bar filled to the score in the band colour,
a plain-language verdict, then eight PASS/FAIL rows with a coloured dot:
Unique sellers · Buy / sell address ratio · LP lock · Mint authority · Buy / sell tax ·
Top 10 holders · Holders · Contract verified.

Verdict copy, in priority order:
ratio > 20:1 -> *"{buyers} addresses bought, only {sellers} ever sold — honeypot pattern.
Trading is gated."*; score >= threshold -> *"High probability of rug or honeypot. Trading
is gated."*; score >= 25 -> *"Mixed signals — seller base is thin relative to buyers. Size
positions carefully."*; else *"No critical failures detected. Buyer and seller address
counts are balanced."*

**Risk gate** (settings, defaults `gateRiskyTrades = true`, `riskGateThreshold = 60`,
range 10–100 step 5): when on and `score >= threshold`, Buy and Zap are disabled,
red-tinted, and read `"Blocked — high risk pool"`. **Selling and removing liquidity are
never gated** — a user must always be able to exit a position.

### 5.7 Portfolio view

Locked state when disconnected: "Connect your wallet" + *"Your holdings, LP positions and
claimable fees appear here once a wallet is connected. Read-only — nothing is signed."*

Connected: four stat tiles (NET WORTH, UNREALIZED P&L with percentage, IN LP POSITIONS,
FEES EARNED), then **Tokens bought** (`{n} tokens held · {total}`) as the holdings table
with a Sell button per row, then **Liquidity positions** (`{n} active positions ·
{total}`) as cards: pair + chain + dex + `In range` / `Out of range` chip, POSITION value,
FEES EARNED, Pool share, Est. APR, and three buttons — Add, Remove,
`Remove + sell → {QUOTE}`.

### 5.8 Exit modal (`Remove + sell`)

Title `Remove + sell`, subtitle `"{pair} · {dex}"`. Caption: *"One transaction: burn LP,
claim fees, sell the volatile side into {QUOTE}. Reverts entirely if any leg fails."*
A `WITHDRAW {n}% of position` slider with 25 / 50 / 75 / 100 presets, the four legs
(1 Burn LP · claim fees, 2 Withdraw A + B, 3 Sell A → B, 4 Settle to wallet), a
**YOU RECEIVE** block in the quote token with its USD equivalent, then Fees claimed /
Sell impact / Remaining position (`Closed` at 100 %) / Est. gas (bundled). Button:
`Remove + sell {n}%` -> `Confirm in wallet…` -> `Settled`, receipt
`"Removed {n}% and sold to {amount} {QUOTE} · 1 tx"`.

### 5.9 Watchlist view

**Token watch** — *"Name a token and EarlyPair alerts you the moment a new pair for it is
deployed on any indexed chain."* Input + **Watch** button. Each entry: name, resolved
address (mono, truncated), status chip (`LISTENING`, or `"{n} new pairs"` in mint),
`"Listening on N chains · added {since}"`, a `×` remove button, and — when there are hits
— matched pairs (chain tag, pair, `"{dex} · {age} ago"`, Trade button). Per-entry empty
state: *"No pair deployed yet — waiting."* A newly added entry appears immediately with
`"resolving symbol…"` in place of the address until the backend resolves it.

**Event topic watch** — *"Add an event signature or topic hash and get every transaction
that emits it across the chains you index."* Input + **Listen** button. Each entry: name,
signature (mono), status chip (`ALERTS ON` amber / `LIVE` mint / `LISTENING` grey),
`topic0 0x…`, `"{n} matches / 24h"` or "No matches yet", and `"last {when} · {txHash}"`
linking to the explorer. Accept either a raw 32-byte `0x…` topic or a human signature such
as `PairCreated(address,address,address,uint256)` — keccak-hash it locally with viem for
instant feedback while the backend confirms.

---

## 6. Formatting rules — implement once in `src/lib/format.ts`

```ts
money(n)  // >=1e6 -> "$1.24M" ; >=1e3 -> "$412.0K" ; else "$842"
num(n)    // n.toLocaleString()
price(v)  // v < 0.01 -> Number(v.toPrecision(3)).toString() ; else v.toFixed(4)
pct(v)    // (v > 0 ? "+" : "") + v.toFixed(1) + "%"
addr(a)   // "0x7a4f…2Be1" — 6 + ellipsis + 4, preserving checksum case
age(ts)   // "2m" | "41m" | "3h" | "1d", recomputed client-side every second
```

Gains use `--accent`, losses `--danger`. Never render a raw float into the DOM. Token
amounts come from the backend as decimal strings — parse with `viem.formatUnits`, never
with `Number()` on a wei value.

---

## 7. Backend contract

REST base `/api/v1`, JSON, cursor pagination (`?limit=&cursor=` -> `{ data, nextCursor }`).
Bearer JWT on portfolio and watch endpoints.

```
GET  /chains
GET  /pools?chain=&risk=&minLiq=&maxAgeMin=&sort=age|liq|vol&limit=&cursor=
GET  /pools/{chainId}/{poolAddress}
GET  /pools/{chainId}/{poolAddress}/risk
GET  /pools/{chainId}/{poolAddress}/traders?window=1h
GET  /rpc/health
POST /rpc/endpoints                  { chainId, transport, url }
POST /rpc/endpoints/{id}/action      { action: "resubscribe" | "ping" | "failover" }
GET  /portfolio/funds | /portfolio/holdings | /portfolio/positions
POST /quote/swap                     { chainId, pool, side, amountIn, slippageBps }
POST /quote/zap                      { chainId, pool, amountIn, splitBps, slippageBps }
POST /quote/exit                     { chainId, positionId, pct, slippageBps }
POST /tx/build                       -> { chainId, to, data, value, gas, expiresAt }
GET|POST /watch/tokens   DELETE /watch/tokens/{id}
GET|POST /watch/topics   DELETE /watch/topics/{id}
POST /auth/nonce   POST /auth/verify
```

Quote responses carry the leg breakdown the UI renders
(`legs: [{ step, label, value }]`), `priceImpactBps`, `estGasUsd`, `minReceived` and an
`expiresAt`. **Re-quote every 10 s while a trade panel is open**; disable the action
button while a quote is in flight rather than letting the user sign a stale route.

**WebSocket** `wss://…/api/v1/stream`. Client subscribe frame:

```json
{ "op": "sub",
  "topics": ["pools.new", "pools.update", "rpc.health", "watch.hits", "head"],
  "filters": { "chains": ["base", "eth"] },
  "since": 918230 }
```

Server frames: `{ "t": "pools.new", "seq": 918233, "ts": 1750000000123, "d": { … } }`.

Implement this in `useEventStream()`:

- exponential backoff with jitter (0.5 s -> 15 s cap), reconnect on `close` and `error`;
- **resume with the last `seq`** so nothing is missed across a reconnect;
- heartbeat: the server pings every 20 s; treat 45 s of silence as dead and reconnect —
  a socket that is open but silent is the common public-infrastructure failure, and it
  will otherwise look healthy forever;
- while disconnected, fall back to polling `GET /pools` every 5 s and show an amber
  "reconnecting — showing polled data" strip under the header;
- expose `status: 'live' | 'polling' | 'down'`; `<BlockPulse>` and the RPC pill reflect it.

---

## 8. Non-negotiables

1. **Never fabricate market data.** If a field is missing, render `—`, not a guess. No
   mock datasets in production builds — the prototype's `POOLS`, `HOLDINGS`, `FUNDS`,
   `RPCS` arrays are design fixtures and must not survive into the app except as
   Storybook/test fixtures.
2. **Non-custodial.** The frontend only signs backend-built transaction payloads, and
   only after showing decoded amounts, minimum received and gas estimate. Always simulate
   (`eth_call` / `simulateContract`) before requesting a signature and surface the revert
   reason instead of letting the wallet fail opaquely.
3. Every funds-moving action shows the exact amounts and the risk chip of the pool it
   touches before the signature request.
4. Accessibility: keyboard-navigable table rows, visible focus rings on every chip and tab
   (`outline: 2px solid var(--accent)`), `aria-live="polite"` on the new-pairs counter,
   `prefers-reduced-motion` respected for row-insert animation.
5. Strict TypeScript. Model every payload in `src/types/api.ts` and validate incoming
   WebSocket frames with **zod** at the boundary — a malformed frame must not crash the
   feed.
6. Performance: the feed can emit several updates per second. Batch cache writes on
   `requestAnimationFrame`, key rows by `` `${chainId}:${poolAddress}` ``, and keep the
   virtualiser mounted across filter changes.

Deliver a working `pnpm dev`, a `pnpm build` that typechecks clean, and a `README.md`
documenting `VITE_API_BASE`, `VITE_WS_URL`, `VITE_WALLETCONNECT_ID`.
