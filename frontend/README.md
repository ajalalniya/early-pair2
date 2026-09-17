# EarlyPair frontend

Vue 3 + TypeScript client for the EarlyPair new-pair discovery and trading terminal. The design reference is `../EarlyPair Standalone.html`. The API it talks to is the Go service in `../backend`.

## Quick start

```bash
pnpm install
cp .env.example .env      # optional, defaults work against a local backend
pnpm dev                  # http://localhost:5173, proxies /api/v1 to EARLYPAIR_BACKEND
```

| Script | What it does |
| --- | --- |
| `pnpm dev` | Vite dev server with API + WebSocket proxy |
| `pnpm build` | `vue-tsc -b` (strict) then `vite build` |
| `pnpm test` | Vitest unit tests (`tests/unit`) |
| `pnpm test:e2e` | Playwright: connect wallet, risk-gated buy, zap in (`e2e/`) |

The e2e suite mocks the backend, the stream socket, public JSON-RPC and an EIP-6963 wallet inside the browser, so it needs no running services. First run: `pnpm exec playwright install chromium`. To capture review screenshots: `EP_SCREENSHOTS=./shots pnpm test:e2e screens`.

## Environment

| Variable | Default | Purpose |
| --- | --- | --- |
| `VITE_API_BASE` | `/api/v1` | REST base URL. Relative in dev (proxied); absolute such as `https://api.earlypair.xyz/api/v1` in production. |
| `VITE_WS_URL` | derived | Stream URL. Empty means `ws(s)://<host of VITE_API_BASE or the page>/api/v1/stream`. |
| `VITE_WALLETCONNECT_ID` | empty | WalletConnect / Reown project id. Without it the WalletConnect option shows as unavailable. |
| `EARLYPAIR_BACKEND` | `http://localhost:8080` | Dev only: where the Vite proxy forwards. Never shipped to the browser. |

The backend must list the frontend origin in `server.corsOrigins` for both REST and the stream's origin check.

## Layout

```
src/
  types/api.ts            zod schemas for every payload; types are inferred from them
  lib/                    format, risk bands/verdicts, chain metadata, CLMM range maths, topic hashing, REST client, wagmi setup
  queries/                TanStack Query hooks; feed.ts owns the pool feed cache and its write helpers
  composables/
    useEventStream.ts     the single WebSocket: backoff, seq resume, 45 s silence watchdog, polling fallback, rAF batching
    useQuote.ts           debounced quotes, re-quoted every 10 s; `ready` gates action buttons
    useTxExecutor.ts      switch chain, simulate with eth_call, then sign, per backend-built step
  stores/                 Pinia: settings (risk gate, accent, slippage), wallet + SIWE session, stream status, toasts
  components/             header, pools, aside (funds, trade tabs, fraud), portfolio, wallet, ui
  views/                  PoolsView, PortfolioView, WatchlistView
```

**Favorite pairs.** The star on each row moves a pair out of the feed into the
Favorite pairs section above the filters; the backend then watches it — live
liquidity, rolling 1h volume, traders and price — until it is unstarred.
Favorites are stored on the backend and shared, so no wallet is needed, and a
star made in one tab reaches the others over the stream. The favorites query
(`queries/favorites.ts`) is the source of truth for whether a pair is starred.
Buyer and seller counts show a `+` while `tradersPending` is above zero: the
backend is still attributing swaps to traders, so the counts are a floor. Rows
removed by the backend's 3-minute liquidity/volume screen fade out of the feed.

**Portfolio.** Every confirmed swap from the Buy / Sell tab is reported to the backend (`lib/tradeLog.ts`), which verifies it from the receipt; the token then appears under "Tokens bought" with its live balance, price, average entry and P&L, refreshed every 10 s while the page is visible and on returning to the tab. The hash is saved to localStorage as soon as the wallet signs, and reports that fail for passing reasons (receipt not visible yet, network) are retried, including on the next visit. Holdings cover trades made here: when a wallet holds more than those explain, P&L shows `—` with the backend's note.

**Pay with ETH.** Buying a pair priced in WETH (or WBNB) shows an ETH | WETH switch in "You pay". ETH is the default, and the choice is remembered in settings. With ETH, quotes and transaction plans carry `payNative: true`, and the router wraps the ETH inside the same swap, so no approval is needed. Sells pay out the pair's own quote token.

**Column filters.** The FILTER row narrows the feed by AGE, LIQUIDITY, VOL 1H, BUYERS / SELLERS and PRICE 1H. Each chip opens a min/max range with presets, and accepts shorthand: `20k` or `1.5m` for dollars and counts, `-20` or `+50%` for price change, and `30m`, `2h` or `1d` for age. A set bound excludes rows whose value is not known yet, since unknown cannot be shown to clear it. Filters combine with chain and risk, live in the URL (`liq=20000~`, `chg=-50~200`), and apply to live-arriving rows too. They filter the loaded feed; when few rows match, older pages are fetched automatically, up to 25 pages.

**Pair search.** The box at the top of the list (focus it with `/`) finds pairs by address — a full address, its first characters (`0x` plus at least 4 hex), or a pasted DexScreener or explorer link. Results come from `GET /pools/search` across every chain and replace the list while the box has text; pairs the backend's screen removed from the feed are found too, marked REMOVED. `Esc` clears it. The query is kept in the URL as `q`.

Filters and the selected pool live in the URL (`/pools?chain=base&risk=safe&sel=base:0xabc…`), so every view can be linked and restored.

## Data rules

- **One source of truth.** REST reads go through TanStack Query. Stream frames are validated with zod and written into the same cache (`setQueryData`), batched per animation frame. A malformed frame is logged and dropped.
- **Nothing is fabricated.** Any field the backend sends as null renders `—`. There is no sample data in `src/`; design fixtures live only in `e2e/fixtures.ts` and `tests/unit/fixtures.ts`.
- **Non-custodial.** The app never asks for a key or seed. Sign-in is one `personal_sign` of the backend's nonce message. Transactions come only from `POST /tx/build`. Each step is simulated before the wallet is asked, and a revert reason is shown instead of a failed prompt. Amounts, minimum received, gas and the pool's risk chip are on screen before signing.
- **Risk gate.** Settings (gear icon): on by default at 60, range 10–100 step 5. It blocks buys and zaps only; selling and removing liquidity are never gated. The threshold is also sent to the backend as `riskThreshold`, so its gate honours the user's setting.
- Token amounts are base-unit decimal strings, parsed and formatted with viem (`parseUnits` / `formatUnits`), never `Number()`.

## Backend features not built yet

The UI for all of these is complete. Each one currently shows the backend's own "not available" message instead of numbers:

| Feature | Backend today | What the UI does |
| --- | --- | --- |
| Buy + LP (zap) | `POST /quote/zap` → 501 `router_not_deployed` | Split slider and leg placeholders render; the button stays disabled until a quote succeeds. |
| Remove + sell | `POST /quote/exit` → 501 | Same pattern inside the exit modal. |
| LP positions | `GET /portfolio/positions` → 501 | The section shows the reason; stat tiles show `—`. The response shape is `PositionSchema` in `types/api.ts`. |
| Liquidity add/remove | no endpoint | Model badge, fee tiers, presets, draggable range and readouts all work; the action is disabled with an explanation. |
| Concentrated range tool | pools carry no spot price | The range tool needs `pool.price` (token0 priced in token1). Until the backend emits it, the tool says so and does not guess. |
| Swap quotes on Uniswap v3 and stable pools | 501 `model_unsupported` (v2 and Uniswap v4 pools trade) | The quote error is shown inline. |

When the router ships, the frontend calls `POST /tx/build` with `{ kind: "zap", chainId, pool, amountIn, splitBps, slippageBps, minReceived, from, riskThreshold }` or `{ kind: "exit", chainId, positionId, pct, slippageBps, minReceived, from }`. The response uses the same `steps` shape as swaps. The current backend rejects unknown fields, so a zap can never be built by mistake as a plain swap.

## Wallets

EVM only, via `@wagmi/core` + viem. The wallet config covers Ethereum, Base, BSC, Arbitrum and Robinhood Chain (id 4663, defined in `src/lib/wagmi.ts`). A chain the backend adds must also be added there before it can be signed for.

- Injected wallets are found through EIP-6963. MetaMask, Phantom (EVM), Rabby and Coinbase show `DETECTED` only when they announce themselves.
- WalletConnect uses `@walletconnect/ethereum-provider`, whose QR modal is Reown AppKit.
- Coinbase smart wallet (passkey) works without an extension.

Solana (`@solana/wallet-adapter`) and SUI (`@mysten/dapp-kit`) are not wired in. The backend ingests EVM chains only, and its SIWE verifies hex addresses only. `@mysten/dapp-kit` is also React-only. The chain chips still list SOL and SUI, marked as not ingesting.

## Accessibility and motion

- Pool rows can be reached by Tab. Enter or Space selects a row; ↑/↓/Home/End move between rows.
- Every chip, tab and button shows a 2 px accent focus ring.
- The "N new pairs ↑" counter is announced through `aria-live="polite"`.
- All animation, including the row-insert flash, is disabled under `prefers-reduced-motion`.
