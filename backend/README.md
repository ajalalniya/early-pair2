# EarlyPair backend

Detects newly deployed liquidity pools within seconds, enriches them, scores
them for fraud, serves them over REST and WebSocket, and builds unsigned
transactions for a non-custodial frontend.

Built to run on **free public RPC endpoints**. Every significant decision below
follows from that constraint.

```
cmd/earlypair-api        REST + WebSocket, scales horizontally
cmd/earlypair-indexer    ingestion + enrichment, one owner per chain
internal/rpcpool         every outbound RPC call goes through here
internal/ingest          the poll loop (the source of truth) and the market tracker
internal/enrich          token metadata and safety probes
internal/risk            the 0-100 score and the eight checks
internal/store           Postgres; the dedupe and cursor guarantees live here
internal/stream          fan-out with sequence-based replay
internal/api             HTTP handlers, quotes, transaction building, auth
internal/pipeline        joins ingest/enrich to the stream and watchlists
```

---

## Quick start

```bash
docker compose up -d postgres redis

export DATABASE_URL="postgres://earlypair:earlypair@localhost:5432/earlypair?sslmode=disable"
export EARLYPAIR_JWT_SECRET="$(openssl rand -hex 32)"

go run ./cmd/earlypair-indexer -config config/earlypair.yaml   # one terminal
go run ./cmd/earlypair-api     -config config/earlypair.yaml   # another
```

Then:

```bash
curl localhost:8080/api/v1/chains
curl 'localhost:8080/api/v1/pools?chain=base&limit=20'
curl localhost:8080/api/v1/rpc/health
go run ./cmd/streamprobe -for 60s     # watch the live WebSocket feed arrive
```

Or the whole stack: `docker compose up --build`. Base images come from AWS ECR
Public's mirror of Docker Hub (identical digests, no Docker Hub login); set
`GO_IMAGE`, `POSTGRES_IMAGE` or `REDIS_IMAGE` in a `.env` file to use another
mirror. The compose project is named `earlypair` explicitly, so it cannot
collide with another project that happens to live in a folder called `backend`.

**Redis is required for the live feed.** The API and the indexer are separate
processes: every new pool, head update and watch hit starts in the indexer and
reaches the API's WebSocket clients over Redis pub/sub. Both binaries refuse to
start if Redis is unreachable, because the alternative is a feed that looks
healthy and is silently empty. `EARLYPAIR_REDIS_DISABLED=true` opts out
explicitly — REST keeps working, live events do not.

Migrations run automatically at startup (`database.autoMigrate`). The indexer
starts from the current head on a cold cursor, so the first pools appear as they
are deployed — within a few seconds on Base, a little longer on Ethereum.

---

## The ingestion decision

**HTTP `eth_getLogs` polling on a persisted block cursor is the source of truth.
WebSocket subscriptions are a doorbell that wakes the poll loop early.** Nothing
is ever written to the database because a socket said so.

Why, for public endpoints specifically:

- **`eth_subscribe` has no replay.** When a public socket drops — provider
  restarts, idle timeouts, connection caps, load-balancer rotation — every log
  emitted during the gap is gone, with no cursor to resume from and no way to
  discover what was missed. A missed `PairCreated` is the product's entire value
  proposition, missed.
- **Silent stalls are the dominant failure.** A public socket will happily stay
  `OPEN` while delivering nothing. Without an independent head source you cannot
  distinguish that from a quiet chain. `internal/rpcpool/ws_test.go` builds
  exactly this server and asserts the supervisor gives up on it.
- **`getLogs` against a cursor is idempotent and gap-free.** A crash, a deploy
  or a partition costs latency, not data.
- **Polling parallelises across providers; a subscription does not.** One
  `getLogs` can be hedged across three endpoints; three unsynchronised
  subscriptions would need the same dedupe layer polling already has.

What the socket buys is the few hundred milliseconds between a block landing and
the next poll tick. If every socket in the fleet dies, detection gets about a
second slower and nothing else changes.

The loop, per chain, in `internal/ingest`:

```
tick  = min(blockTime/2, 1s)                fallback timer
wake  = tick | newHeads | matching logs     doorbell, debounced 150ms

on wake:
  head = quorum head across healthy endpoints   (never one endpoint's word)
  verify the cursor block is still canonical; rewind if not
  for each chunk up to head:
      getLogs(from, to, factoryAddresses, creationTopics)   must succeed
      getLogs(from, to, no address, watchedTopics)          skipped if refused
      (each tried across endpoints; ranges bisected when too large)
      decode, insert (dedupe absorbs overlap), emit
      persist the cursor
```

Specifics that matter:

| Behaviour | Where |
|---|---|
| `getLogs` bisected on a refused range, working size remembered per endpoint | `ingest.sweep`, `Endpoint.ShrinkLogRange` |
| Pools enter at 0 confirmations, re-verified at depth, `reorged` if the log is gone | `ingest.confirmPass` |
| Reorg rewind walks back past gaps; full-depth rewind if nothing agrees | `ingest.checkReorg` |
| Dedupe at the store boundary: `UNIQUE (chain_id, created_tx, log_index)` | `migrations/0001_init.sql` |
| A rewound pool that is still there is **restored**, not re-announced | `store.RestorePool` |
| One goroutine tree, rate budget and breaker per chain | `cmd/earlypair-indexer` |
| One indexer per chain, via a Postgres advisory lock | `store.TryChainLock` |

Each range is two queries, not one. **Pool creations are fetched with the
factory addresses as a filter** — every provider accepts that, and it is the
cheaper request because providers index logs by address. **Watched event topics
are a separate, address-less query**, because they have to match any contract.
They were originally folded into one address-less sweep; live, publicnode
refused that shape outright ("Please specify an address in your request") and
pool detection on BSC and Arbitrum stalled completely. Now a watch query no
endpoint will serve is skipped and counted (`watch_topic_sweeps_skipped_total`)
and never holds the creation cursor back: the pool feed is the product, a topic
watch is a feature on top of it.

Within a range the sweep **fails over across endpoints** rather than failing the
tick. An endpoint that refuses a query shape on its tier — address-less logs, or
"archive requests require a personal token" once a catch-up falls behind the tip
— is remembered per endpoint and per shape (`Endpoint.Refuses`) and routed
around, while still being used for everything it does serve. This is kept apart
from the circuit breaker deliberately: those endpoints answer `eth_blockNumber`
perfectly, so each head check reset the breaker and a method-specific refusal
could never trip it. The health panel lists active refusals as `limitations`.

Archive refusals are **depth-aware and need three strikes**. Measured live,
publicnode served Ethereum logs 300 and 2,500 blocks behind the tip but refused
them 1,000 back — its backends keep different history — and drpc serves ~100
blocks back and refuses beyond ("Can't route your request to suitable
provider"). So a refused endpoint is skipped only for queries at least as deep
as the ones it refused, a plain log refusal is retried once immediately, and it
takes three to route around. Before this, one unlucky request barred Ethereum's
only working log source and the chain sat ~2,400 blocks behind; after, it
caught up in four minutes. Refusals and range-too-large answers also no longer
count against an endpoint's health, which had let a catch-up trip the breaker
on an endpoint serving its head checks perfectly.

Pool **orientation** is decided by `chain.Orient`, not slot order. `token0` is
just the lower address, so WETH lands there for many new tokens; treating slot 0
as the traded token named those pairs backwards, counted every seller as a buyer
(inverting the honeypot signal), and built the opposite trade for a "buy".

A pool whose block is more than five minutes old when detected is marked
`backfilled`: stored and served, but not pushed to clients as `pools.new` and
not recorded in `detection_latency_seconds`. After downtime the indexer catches
up without burying what is actually new under hours of history.

Re-verification at confirmation depth reads each run of nearby creation blocks
with one query, not one query per block. At Robinhood's 0.1s blocks the
per-block form alone outran its endpoint's rate budget.

### Market columns: the tracker

`liquidityUsd`, `volume1hUsd`, `buyers1h` / `sellers1h` and `priceChange1h` are
filled by the market tracker (`internal/ingest/tracker.go`). It watches each new
pair, **screens** it three minutes in, and keeps tracking the survivors until
they are ten minutes old (`ingest.trackWindow`); then their figures freeze.
**Favorites** are watched for as long as they stay starred. Refreshing every
pool ever seen cost a set of calls per pool forever, and on free endpoints the
columns stayed empty.

**Favorites** (`favorite_pools`, `GET /favorites`, `PUT|DELETE
/favorites/{chain}/{pool}`). Starring needs no sign-in; the list is shared and
capped at 50 (`409 favorites_full`), because every favorite is watched out of
the same public RPC budget that discovers pools. A favorite is never frozen and
never screened. Starring a pair the tracker is not watching — older than the
window, or frozen — makes it read that pair's own events, from creation, at
most 24 hours back and two queries a tick, before its figures are published
again. A v4 pair older than that cap has unknown liquidity, since its early
positions were never read. Starring and unstarring push a `pools.update` frame
`{"chainId","address","favoritedAt"}` (null when unstarred).

**Screening** (`ingest.screen`). At `after` (3m), a pair is removed if its
liquidity is under `minLiquidityUsd` ($10k) *or* its volume is under
`minVolumeUsd` ($100) — `keepIf: both`, the default. `keepIf: either` removes
only pairs short on both. A dollar figure that cannot be established does not clear. The verdict waits
for figures that are only late: a scan that has not yet read past the screening
time, a native quote before the native price has loaded, v3 balances being read.
It waits at most two minutes. A removed pair gets `removed_at`: it leaves
`/pools`, enrichment and confirmation, and clients receive a `pools.update`
frame `{"chainId","address","removed":true,"removedAt"}`. The row is kept, so
the creation dedupe still holds and `GET /pools/{chain}/{pool}` and transaction
building still work for anyone holding it. `screened_at` records the verdict, so
a restart does not judge a survivor again on later figures. Over one live hour,
9% of tracked pairs cleared the both bar (23% the either bar). A pair rugged
after its screening stays listed with `liquidityUsd: 0`.
`pairs_screened_total{chain,outcome}`
counts verdicts.

- **One query per tick per chain** (`ingest.trackEvery`, 3s) covers every pair
  still in its window: pair contracts by address, v4 pools on the PoolManager by
  PoolId. Everything is derived from those events.
- **Price change** runs from the pair's first price (`Initialize` or first
  `Sync`) to its latest (`Sync` or the swap's `sqrtPriceX96`), in the traded
  token's quote.
- **Volume** is the quote side of every swap, in dollars.
- **Liquidity** values both sides at the latest price. v2 uses reserves from
  `Sync`, v3 the pool's token balances (one multicall), and v4 the sum of
  `ModifyLiquidity` positions converted at the current tick. A pair whose
  liquidity was pulled shows `0`; that is the figure, not a gap.
- **Buyers / sellers** are distinct transaction signers, because routers sit in
  every event's sender field. Each signer lookup is one request: Robinhood's
  endpoint meters every element of a batch and answered 429 after 150. Lookups
  are budgeted per tick at 40% of the chain's HTTP rate and shared round-robin
  across pairs. An unbudgeted lookup stalled the tick for minutes and took
  discovery's budget. Oldest-first let one busy pair hold every other pair at
  zero. On busy chains lookups trail the swaps, and **`tradersPending`** says by
  how much: while it is above zero, `buyers1h` / `sellers1h` are a floor (show
  "27+"), not a final count. Counts are never written lower than before, so a
  restart does not make them fall. A frozen pair keeps resolving its swaps for
  up to 10 more minutes. `market tracker: trader lookups are behind` is logged
  while lookups lag; on Robinhood's single endpoint the backlog grows, and a
  second endpoint is what would close it.

The 1H columns are a **rolling hour**, measured in blocks from the latest block
read. Swaps older than that leave volume and the trader counts, and price change
runs from the last price at or before the hour mark. For a pair younger than an
hour — every non-favorite — that is "since launch"; after its window a pair
keeps the figures it had then. `metricsUntil` (null for favorites) and
`metricsFrozen` on every pool say which applies.

Dollar figures need a dollar price for the quote asset. Stables are $1. Native
and wrapped native use the chain's reference pool, or a chain with the same
native asset (Robinhood and Arbitrum read ETH from Ethereum or Base). A chain
paying gas in a dollar stablecoin sets `nativeIsStable` and needs no reference
pool: Arc's gas is USDC, whose ERC-20 interface at 0x3600…0000 is also what its
pools pair against, and it has no wrapped native token at all. Any other
quote leaves `liquidityUsd` and `volume1hUsd` null while price change and traders
still fill in. Backfilled pairs are never tracked and keep all four columns null.

### Uniswap v3

Quotes come from Uniswap's **QuoterV2**, which runs the swap inside the pool, so
every tick crossed and the fee tier in force are accounted for. No price is ever
computed from pool state here: a v3 pool's liquidity is whatever range orders
sit around the current price, so there is no constant-product formula to apply,
and an estimate that ignored the ranges would be wrong by an amount that depends
on where the price sits.

Swaps go through **SwapRouter02**. Its `exactInputSingle` carries no deadline of
its own, so the swap is wrapped in `multicall(deadline, calls)`: a transaction
left unsigned in a wallet cannot be mined later at a price that has moved. One
approval to the router covers the input — there is no Permit2 leg, unlike v4 —
and it is written for the wallet's balance, so the next trade needs none.

The pool's **fee tier is part of its identity**, not a setting: the router finds
the pool from (tokenIn, tokenOut, fee), so a pool whose tier is unknown is
refused rather than quoted against a tier that addresses a different pool.

Contracts sit under `uniswapV3:` per chain and are confirmed on-chain the same
way as v4's: `factory()` on both the quoter and the router must return the v3
factory configured for that chain. A v3 **fork is not interchangeable** —
PancakeSwap v3's pools live in their own factory, and pricing one through
Uniswap's quoter finds no pool at all — so routing matches the pool's dex name
against `uniswapV3.dex` (default "Uniswap v3"). Paying with native currency is
not offered on v3: it needs the router's wrap-and-refund sequence, and Arc, the
chain this was built for, has no wrapped native at all.

### Uniswap v4

v4 is active on Ethereum, Base, Arbitrum, BSC and Arc. The PoolManager addresses,
the eight-argument `Initialize` signature and the `Swap` sign convention were
all checked against live chains before being encoded (identical PoolManager
bytecode on all four; real `Initialize` events on Base). What differs:

- **Pools are PoolIds, not addresses.** A v4 pool's `address` in the API is its
  32-byte PoolId (`0x` + 64 hex), and `pools.hooks` holds its hook contract.
- **Native currency.** `currency0` is often the zero address — native ETH/BNB,
  no wrapper. It is named from the chain descriptor, priced like the wrapped
  native, and treated as a quote asset for orientation.
- **Swaps come from the PoolManager, filtered by PoolId, and the sign is
  flipped.** v4 reports the swapper's side where v3 reports the pool's; in 341
  of 341 live Base swaps a positive `amount0` moved token0's price up. Decoding
  them with v3's convention would invert buyers and sellers.
- **Traders are transaction signers.** The event's sender is the Universal
  Router for almost every trade, so each distinct transaction's signer is looked
  up (`eth_getTransactionByHash`); see the market tracker above.
- **Dynamic fees** (flag `0x800000`) report `feeTierBps: null` — the hook sets
  the fee per swap, so there is no tier to show.
- **Hooks are named, not scored.** On Base, 7 of 13 new pools had a custom hook.
  A hook runs on every swap and can take fees or refuse sells, invisibly to the
  token contract. Nothing here audits hook code, so a hooked pool's summary lists
  `"Custom v4 hook (not audited)"` in `risk.unavailable`; it adds no points.
- **LP lock is "not available" for all concentrated-liquidity pools** (v3 and
  v4): positions are NFTs, with no fungible LP token to read a lock from.
  Previously this left every such pool stuck in `PENDING`.
- **Trading.** v4 pools are quoted and traded (`internal/uniswapv4`). The pool's
  PoolKey is read from its Initialize event and must hash to its PoolId. Quotes
  come from Uniswap's V4Quoter, which runs the swap in the PoolManager, so the
  hook, the fee in force and every tick crossed are included; price impact is
  measured against a quote a thousand times smaller. Swaps go through the
  Universal Router as one `V4_SWAP` (`SWAP_EXACT_IN_SINGLE`, `SETTLE_ALL`,
  `TAKE_ALL`), with minimum received enforced by the router. Paying native
  currency carries it as `value` and needs no approval; paying a token adds up
  to two approval steps, token→Permit2 and Permit2→router. Each is written for
  the wallet's whole balance of that token, so the next trade in it needs no
  approval — signing two transactions before every buy is the kind of friction
  that loses the trade. Sized to the balance rather than unlimited, and the
  Permit2 leg expires after 30 days: an approval that is forgotten cannot
  authorise more than the wallet held when it was signed, and lapses on its own.
  `POST /tx/approve` builds only those approvals, for a trade that has not
  happened yet: signed ahead, the sell itself is then one transaction, which is
  what matters when the moment to sell a new pair is measured in seconds.
  Holdings report `sellReady` (null when the allowances could not be read) so
  the portfolio can offer it only where it does something. The swap builders,
  this endpoint and the flag share one implementation of the Permit2 rules
  (`approvalSteps`), rather than three copies that could drift apart.
  Contracts are per chain under `uniswapV4:`, Uniswap's published
  addresses, each confirmed on-chain (`poolManager()` on quoter and router).
  **Robinhood Chain's and Arc's routers take an older layout**: their
  exact-input params carry one more `uint256` before `hookData`
  (`extraSwapParam: true`; Robinhood calls it `minHopPriceX36`).
  The layouts were told apart on a pool whose currency0 is a token: the right
  one reaches Permit2 (`AllowanceExpired`), the wrong one fails decoding. On
  native-quoted pools both happen to decode, because the misread offset lands on
  currency0's zero word. `go run ./cmd/v4probe -chain … -pool … -block …`
  repeats the check without spending anything.
- **Paying ETH on WETH pairs** (`payNative: true` on `/quote/swap` and
  `/tx/build`, buys only, and only when the pair is priced in the chain's wrapped
  native token). On v4, the router runs `WRAP_ETH` into itself and then
  `V4_SWAP` with `SETTLE` paid by the router instead of `SETTLE_ALL` through
  Permit2; the value carries the input, and no approval is needed. On v2, the
  swap is `swapExactETHForTokensSupportingFeeOnTransferTokens` on the chain's v2
  router. Both were simulated before shipping, with a state-override balance:
  Robinhood and Base v4 WETH pools, Base and Ethereum v2 pairs (each router's
  `WETH()` matches the chain's `wrappedNative`). A 2× minimum reverts, so the
  minimum received is still enforced. `v4probe -wrap` repeats the v4 check.
  Selling still pays out WETH.

v2 quotes and portfolio prices read reserves live with `getReserves`. They
used to come from the `pool_reserves` table, which nothing has written since
the market tracker replaced the enrichment market stage, so every v2 quote had
failed with "reserves are not known yet".

`Initialize` creates a pool without funding it, and v4 pool creation is noisy:
during one nine-hour Ethereum catch-up, 1,150 pools came from 310 transactions,
up to 20 per transaction. They are stored and enriched like any other pool;
there is no spam filter.

## The RPC pool

Business logic never dials a node. Everything goes through `internal/rpcpool`,
which is the only place a public provider's behaviour has to be understood:

- **Cost classes.** Cheap / normal / expensive draw different amounts from an
  endpoint's token bucket, so a log sweep cannot starve a quote.
- **Hedging.** The best endpoint gets the request; if it has not answered within
  the hedge delay, the second-best races it and the first valid answer wins.
- **Quorum head.** `max(heads)` excluding endpoints implausibly far ahead of the
  median — a public node occasionally reports a head from the future, and
  trusting it would park the cursor past the real tip and silently stop the feed.
- **AIMD on 429.** Halve the rate, recover linearly over five minutes. The burst
  is deliberately never shrunk below the largest class cost: a bucket that can
  never hold four tokens *rejects* four-token calls rather than delaying them.
  (That was a real bug; `TestRateLimitDoesNotWedgeExpensiveCalls` pins it.)
- **Socket demotion.** Two drops inside ten minutes sends an endpoint to
  HTTP-only for fifteen — exactly what the UI tells users happens.
- **Background lane.** Enrichment marks its calls with `rpcpool.Background`, which
  caps them at half of each endpoint's rate. Discovery and market tracking always
  get the rest. Without it, enrichment's log probes took nearly all of Robinhood's
  single endpoint, and the cursor fell 40 minutes behind. Every pair found that
  late counted as backfilled and was never tracked.
  (`TestBackgroundWorkLeavesDiscoveryItsShare`.)
- **Masked URLs.** An endpoint URL reaches the health panel and every warning
  line. `config.MaskURL` is applied everywhere; `TestMaskURLNeverLeaksASecret`
  checks it against realistic keyed URLs.

## Risk scoring

Eight checks, the ones the UI renders, with the design's pass conditions:

| Check | Passes when |
|---|---|
| Unique sellers | `> 20` |
| Buy / sell address ratio | `< 8` |
| LP lock | `Locked…` or `Burned` |
| Mint authority | revoked or renounced |
| Buy / sell tax | `0 / 0` |
| Top 10 holders | `< 25 %` |
| Holders | `> 100` |
| Contract verified | source published |

Weights live in config so a newly observed rug pattern can be reweighted without
a deploy, and every score is stored with the inputs that produced it
(`risk_snapshots`) so it stays explainable and can be re-run against new weights
without re-reading the chain.

Properties enforced in code and in tests:

- **An unknown input is not a pass.** A fact we have not established contributes
  nothing to the score and reports "not checked yet" with `known: false`, and the
  summary carries `complete: false`.
- **A low score from missing evidence is `PENDING`, not `SAFE`.** Bands are
  `PENDING`, `SAFE`, `WATCH`, `DANGER`. A score under 25 with any check still
  pending is `PENDING`. `WATCH` and `DANGER` need no such guard — missing
  evidence can only add points — so a partial score that is already high still
  shows. This was caught in the live feed: new pools with no trades read
  `SAFE 21`.
- **No trades is not a honeypot tell.** Zero buyers and zero sellers reports
  "no trades yet" on both trader checks instead of failing "no exit observed".
- **Unavailable is not pending.** Checks this deployment cannot perform at all —
  the tax/honeypot simulation, source verification without an explorer key, and
  LP lock on concentrated-liquidity pools — report "not available" and are
  listed in `risk.unavailable`, along with an unaudited v4 hook. Waiting would
  never fill them in, so they must not hold every pool in `PENDING` forever; but
  they are named, so a `SAFE` band never implies they ran.
- **A reverting sell gates on its own.** `honeypotSimFailed` is weighted above
  the gate threshold, because a position that cannot be exited is not a matter of
  degree.

`internal/risk/risk_test.go` runs the design prototype's twelve sample pools as
golden cases. It asserts the **band** each lands in rather than the exact score:
the prototype's numbers are hand-authored design values, so matching them
digit-for-digit would only prove we reverse-engineered someone's illustrative
constants. All twelve bands agree.

Gating applies to **buys only**. Sells and liquidity removals are never refused,
at any score — a user must always be able to exit.

## API

Base `/api/v1`. Cursor pagination (`?limit=&cursor=` → `{data, nextCursor}`).
Bearer JWT, issued by signing a nonce, on portfolio and watch routes.

```
GET  /chains
GET  /pools?chain=&risk=&minLiq=&maxAgeMin=&sort=age|liq|vol&limit=&cursor=
GET  /pools/{chainId}/{pool}            /risk    /traders
GET  /pools/search?q=0x…                 address or prefix (≥4 hex), all chains, removed pairs included
GET  /favorites     PUT|DELETE /favorites/{chainId}/{pool}
GET  /rpc/health
POST /rpc/endpoints                     /rpc/endpoints/{id}/action
POST /auth/nonce                        /auth/verify
POST /quote/swap  /quote/zap  /quote/exit
POST /tx/build    /tx/send
GET  /portfolio/funds | /holdings | /positions
GET|POST /watch/tokens  DELETE /watch/tokens/{id}
GET|POST /watch/topics  DELETE /watch/topics/{id}
GET  /stream            (WebSocket)
```

Every pool carries `metricsUntil` (creation + the tracking window; `null` for
backfilled pools and favorites), `metricsFrozen`, `screenedAt` (`null` until judged), `favoritedAt`,
`tradersPending`, and `removedAt` on a removed pair fetched directly. `pools.update`
frames are pushed whenever the tracker writes new figures, and once with
`"removed": true` when a pair fails screening.

**Stream.** Subscribe with
`{"op":"sub","topics":[…],"filters":{"chains":[…]},"since":918230}`; frames are
`{"t":…,"seq":…,"ts":…,"d":…}`. A monotonic `seq` plus a five-minute replay
buffer makes reconnects lossless — and when a client has been away longer than
the buffer reaches, it is told to resync rather than handed a partial history
that looks complete. Slow clients are dropped and flagged for resync rather than
buffered without bound. `pools.update` and `head` are coalesced on a 250 ms
window.

Events cross from the indexer to the API over Redis pub/sub, which is
at-most-once. Every bus message carries its producer's identity and sequence
number; an API instance that sees a gap sends its clients a `resync` frame
(delivered regardless of topic subscription) rather than letting them believe
their feed is complete. The replay `seq` is per API instance, so a client that
reconnects to a *different* instance should refetch.

**Health and operator actions.** `/rpc/health` serves the indexer's endpoint
snapshot (`"source": "indexer"`) — those are the endpoints carrying detection —
and falls back to the API's own view with an explanatory `note` only when no
recent snapshot exists. The panel's Ping / Resubscribe / Failover actions are
sent to the indexer over the bus; the response is `503 no_indexer` if nothing is
listening.

**Transactions.** `/tx/build` returns unsigned payloads plus an explicit approval
step when allowance is short, simulates before returning, and refuses to hand
over a transaction whose simulation reverts (returning the decoded reason). The
service holds no keys and cannot sign. `/tx/send` relays an already-signed raw
transaction for clients on restricted networks.

## What is not implemented

Listed plainly, because each is a deliberate refusal rather than an oversight,
and each returns an explicit error instead of a plausible-looking number.

- **Solana and SUI ingestion.** Configured but disabled. The doctrine ports
  directly — `getSignaturesForAddress` with a signature cursor, `suix_queryEvents`
  with a checkpoint cursor, sockets as doorbells — but the ingesters do not
  exist. They are disabled rather than enabled-and-empty so the UI can say "not
  indexed" instead of showing what looks like "no new pools".
- **Zap and exit quotes** (`501`). Both are single-transaction, multi-leg
  operations, and "reverts entirely if any leg fails" is a promise only an
  on-chain router can keep. No such contract is deployed for this service, and
  assembling the legs client-side would break exactly the atomicity the UI
  advertises.
- **Stableswap quoting** (`501`). Needs the curve's own math; a constant-product
  estimate against such a pool is wrong by an amount that depends on the curve.
  (Uniswap v3 is quoted and traded — see below.)
- **Buy/sell tax and the honeypot simulation.** Requires `eth_call` state
  overrides that several free endpoints do not support, and cannot be verified
  correct without running against live chains. An unverified simulator would
  produce confident tax percentages that might be wrong — the figure a user is
  most likely to act on. The check reports "not available" and is named in
  `risk.unavailable`; the storage and read-back path is in place for when the
  probe can be validated.
- **LP positions** (`501`). Need per-wallet mint/burn indexing across DEX
  generations. `/portfolio/funds` reads balances directly from chain.
- **Holdings cover trades made through EarlyPair only.** After a swap confirms,
  the client posts its hash to `POST /portfolio/trades` (no session needed). The
  backend verifies it from the mined receipt: success, sent to this chain's v2
  router or Universal Router, and a Swap event in the named pool. It records
  the amounts, taking the wallet's actual Transfer for taxed tokens, plus the
  quote's dollar value at recording. `GET /portfolio/holdings` reads each
  token's balance live, prices it with a small sell quote into its pool, and
  takes the average entry from verified buys. When the balance exceeds what
  those trades explain (bought elsewhere, sent in), P&L is left null with a
  `note` rather than computed from part of the history.
  `DELETE /portfolio/holdings/{chain}/{token}` removes a token from the list
  for the session wallet. Its trades are kept, so a trade recorded later lists
  it again with its full cost basis. Migrations take a
  Postgres advisory lock, since the API and indexer both migrate at start.
- **Curve factories.** Their creation events do not carry the pool address, so
  they need receipt-level resolution. `chain.ErrUnsupportedEvent`; not configured.
- **Market figures and screening for backfilled pools**, and figures after the
  tracking window. Pools found by a catch-up are neither tracked nor screened;
  they stay listed with empty columns. Tracked pools stop updating at ten
  minutes unless starred.
- **Endpoints added at runtime reach the indexer on restart.** `POST
  /rpc/endpoints` persists the endpoint and adds it to the API immediately; the
  indexer loads it at startup.

Prices come from each chain's configured reference pool (`nativePriceRef`) — the
service carries no external price feed, so every figure it shows traces to a
chain it already reads. A price older than ten minutes is reported as
unavailable rather than used.

## Deviations from the brief

- **No ClickHouse.** The brief allowed "ClickHouse *or* a Timescale hypertable".
  This uses plain Postgres with a time index, keeping the stack to two services;
  the migration marks where `create_hypertable` goes, and the same queries work
  unchanged. Per-swap rows are no longer written; the tracker keeps what it
  needs in memory for each pair's window.
- **Uniswap v4's `Initialize` signature.** The brief listed a six-argument form.
  The deployed `PoolManager` event takes eight; matching the six-argument form
  would yield no pools. `chain.SigInitializeV4` uses the canonical one, and
  `TestKnownTopicHashes` pins the derived topics against explorer-checkable
  values.
- **The risk JSON gained fields and a fourth band.** `RiskCheck.known`,
  `RiskSummary.complete` and `RiskSummary.unavailable` are additive. The
  **`PENDING` band is not**: a frontend built for exactly three bands must learn
  to render it (neutral, unscored). Without it a pool with no evidence reads as
  `SAFE`, which the correctness rule forbids.
- **`/rpc/health` gained `source`, `asOf`, and per-endpoint `limitations`.**
  Additive.

## Configuration

`config/earlypair.yaml`, with env overrides. Secrets never go in the file.

| Variable | Purpose |
|---|---|
| `DATABASE_URL` | Postgres connection string |
| `REDIS_URL` | Redis connection string; **required** for the live feed |
| `EARLYPAIR_REDIS_DISABLED` | `true` to run without Redis: REST works, no live events |
| `EARLYPAIR_JWT_SECRET` | **Required by the API**; it refuses to start without one |
| `EARLYPAIR_ADDR`, `EARLYPAIR_METRICS_ADDR` | Listen addresses |
| `EARLYPAIR_CORS_ORIGINS` | Comma-separated allowed origins |
| `EARLYPAIR_LOG_LEVEL`, `EARLYPAIR_LOG_FORMAT` | `debug\|info\|warn\|error`, `text\|json` |
| `EARLYPAIR_GATE_THRESHOLD` | Default risk gate score |
| `EARLYPAIR_RPC_URL_<ID>` | Override one endpoint's URL (id upper-snaked) |
| `EARLYPAIR_EXPLORER_KEY_<CHAIN>` | Explorer API key for source verification |

**Adding a chain.** Add a `chains:` entry (`kind: evm`, block time, confirmation
depth, `multicall3`, `wrappedNative`, `stables`, `nativePriceRef`) and at least
one **HTTP** endpoint — config validation rejects an enabled chain with only a
socket, because polling is the source of truth and cannot run on sockets alone.

**Adding a DEX.** Add a `factories:` entry with its address, `model`
(`v2` / `clmm` / `stable`) and `event` (`pair_created_v2`, `pool_created_v3`,
`initialize_v4`, `pool_created_solidly`). The `model` drives the frontend's
entire liquidity panel. A new event shape needs a decoder in
`internal/chain/events.go`.

Users and operators can also add endpoints at runtime via `POST /rpc/endpoints`;
those are persisted and reloaded at startup.

## Observability

`/metrics` on `:9090` (API) and `:9091` (indexer). The ones worth alerting on:

- `cursor_lag_blocks{chain}` — the earliest signal that ingestion is failing.
- `detection_latency_seconds{chain}` — block timestamp to client frame; the
  number the whole design exists to keep small.
- `rpc_rate_limited_total{endpoint}`, `ws_reconnects_total{endpoint}` — when a
  provider is turning against you.
- `rpc_capability_refusals_total{endpoint,capability}` — a provider changed its
  free-tier policy and traffic is shifting onto the rest of the fleet.
- `bus_sequence_gaps_total`, `bus_messages_dropped_total` — live events lost
  between indexer and API; each gap triggers a client resync.
- `pairs_detected_total`, `reorgs_total`, `enrichment_queue_depth`,
  `getlogs_range_shrinks_total`, `stream_dropped_clients_total`,
  `watch_topic_sweeps_skipped_total`.

`/healthz` is liveness; `/readyz` checks the database.

## Tests

```bash
go test ./...              # full suite, ~16s (one socket test waits out a silence window)
go test -short ./...       # skips it
```

The ingester is tested against a simulated chain behind a deliberately hostile
provider (`internal/ingest/fixture_test.go`) that throttles one request in four,
refuses any `getLogs` range wider than it feels like, and returns a 500 one
request in nine. The assertions are the guarantees:

- `TestIngestUnderHostileProvider` — every pool found, each exactly once, and the
  test fails if the provider did *not* actually misbehave.
- `TestReplayIsIdempotent` — re-reading a processed range emits nothing new.
- `TestReorgRewindsAndRescans` — the orphan is marked reorged and clients are
  told; the surviving pool is not announced twice.
- `TestCursorSurvivesTotalProviderFailure` — with every request failing the
  cursor does not move, and the deferred work completes on recovery.
- `TestAddresslessRefusalDoesNotStallDetection`,
  `TestLogRefusingEndpointIsRoutedAround` — the two refusals publicnode produced
  live, reproduced with its exact error text; the preferred endpoint refuses and
  the one behind it carries the chain, asked once rather than every tick.
- `TestWatchSweepNeverBlocksCreation` — no endpoint serves the watch query;
  pools are still detected.
- `TestCatchUpPoolsAreNotAnnouncedAsNew` — only pools inside the live window
  are announced.

`internal/api/quote_test.go` pins orientation against the pool shape seen live
on Base (`WETH / 1KTO100M`): a buy pays WETH and receives the token.

Plus the silent-but-open socket (`internal/rpcpool/ws_test.go`), the replay and
backpressure guarantees (`internal/stream/hub_test.go`), topic derivation against
explorer-checkable hashes (`internal/chain/events_test.go`), and the shipped
config (`internal/config/config_test.go`).

## Non-negotiables

1. **No key material.** None is stored, requested or transmitted. Transactions
   are built unsigned and signed client-side.
2. **Correctness over latency at the data layer.** A pool detected 1.5 s late is
   fine; a pool missed, duplicated or silently wrong is not.
3. **Never present derived data as certain.** Failed enrichment returns `null`
   and renders as an em dash. A fabricated liquidity figure is worse than a
   missing one, because the user trades on it.
4. **Budget the RPC.** Every call goes through `rpcpool` with a declared cost
   class. No direct `ethclient` use from business logic.
5. **Exits are never gated.** The API may refuse to build a buy above the
   caller's threshold; it must always build sells and liquidity removals.
