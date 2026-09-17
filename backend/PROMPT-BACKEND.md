# EarlyPair — Backend Build Prompt (Go)

You are building the backend for **EarlyPair**, a multi-chain new-pair discovery and
trading terminal. It must detect newly deployed liquidity pools within seconds of the
creating transaction landing, enrich them, score them for fraud, serve them over REST and
WebSocket, and build unsigned transactions for a non-custodial Vue frontend.

The design prototype (`EarlyPair Standalone.html`) defines the exact fields the UI
expects — read §6 for the contract you must satisfy.

**Hard constraint: this runs on free public RPC endpoints.** Every design decision below
follows from that. Public endpoints rate-limit aggressively, cap `eth_getLogs` ranges,
drop WebSockets without warning, silently stall subscriptions while keeping the socket
open, and serve stale heads. The architecture must assume all of that is normal.

---

## 1. Stack

- **Go 1.23+**, standard project layout: `cmd/earlypair-api`, `cmd/earlypair-indexer`,
  `internal/{chain,ingest,enrich,risk,store,api,stream,rpcpool,quote}`.
- HTTP: **chi** (or net/http with Go 1.22 routing). WebSocket: **coder/websocket**
  (formerly nhooyr) — context-aware, no goroutine-per-write footguns.
- EVM: **github.com/ethereum/go-ethereum/ethclient** + `rpc` for batch calls and raw
  subscriptions. ABI binding via `abigen` only for the factory/pair/pool events.
- Solana: **gagliardetto/solana-go**. SUI: plain JSON-RPC over HTTP (`suix_*`).
- Storage: **PostgreSQL 16** via **pgx/v5** (pools, tokens, risk, watchlists, cursors) +
  **ClickHouse** or a Timescale hypertable for the swap-event firehose (1h trader counts,
  volume). **Redis** for hot state, per-endpoint rate-limit budgets and cross-instance
  pub/sub fan-out.
- Observability: **prometheus/client_golang**, **slog** with structured fields
  (`chain`, `endpoint`, `block`, `latency_ms`), **OpenTelemetry** traces on ingest paths.
- Config: env + YAML per chain. Migrations: **goose**.
- `errgroup` for supervision, `golang.org/x/time/rate` for limiters, `singleflight` for
  dedup of concurrent identical RPC reads.

---

## 2. The ingestion decision — read this first

**Use both, but they are not peers. HTTP `eth_getLogs` polling on a persisted block
cursor is the source of truth. WebSocket subscriptions are a latency hint that triggers
an immediate poll.** Never insert a pool into the database directly from a WebSocket
payload.

Why, specifically for public RPCs:

- **WebSocket gives you no replay.** When a public socket drops (and they drop — provider
  restarts, idle timeouts, connection caps, LB rotation), any logs emitted during the gap
  are gone. There is no resume cursor in `eth_subscribe`. A missed `PairCreated` is a
  missed product event, which for this product is the whole value proposition.
- **Silent stalls are the dominant failure mode.** A public socket will frequently stay
  `OPEN` while delivering nothing. Without an independent head source you cannot tell
  "quiet chain" from "dead subscription". Polling detects this for free.
- **`eth_getLogs` is idempotent and gap-free.** With `lastProcessedBlock` persisted, a
  crash, a deploy, or a 30-second network partition costs you latency, not data. You
  simply resume from the cursor.
- **Polling parallelises across providers; a subscription does not.** You can hedge one
  `getLogs` call across three endpoints and take the first good answer. You cannot hedge a
  subscription — you would have to dedupe three unsynchronised streams anyway, which is
  exactly the dedupe layer polling already needs.
- **What WebSocket is genuinely good for** is shaving the polling interval off detection
  latency. A `newHeads` push arrives ~200–800 ms before your next poll tick would fire.
  So use it as a *doorbell*: on any `newHeads` or matching `logs` frame, wake the poll
  loop for that chain immediately. You get sub-second detection with getLogs-grade
  completeness, and if every socket in the fleet dies the system degrades to
  "1–2 seconds slower", not "broken".

Concretely, per chain:

```
tick := min(blockTime/2, 1s)              // fallback timer
wake := merge(tick, wsNewHeads, wsLogs)   // doorbell channel, debounced to 150ms

on wake:
  head = maxHeadAcrossHealthyEndpoints()  // never trust one endpoint's head
  if head <= cursor { return }
  for from := cursor+1; from <= head; from += chunk {
      logs = hedgedGetLogs(from, min(from+chunk-1, head), factoryTopics)
      emit(logs)                           // dedupe key: chainID:txHash:logIndex
      cursor = to; persistCursor(cursor)
  }
```

Rules the poller must honour:

- **Chunk `getLogs` to <= 500 blocks** and shrink adaptively. On
  `-32005 / "query returned more than N results" / "block range too large" / response
  too large`, **bisect the range and retry** rather than failing the tick. Remember the
  working chunk size per endpoint.
- **Process at head with 0 confirmations** so the UI is fast, marking rows `unconfirmed`.
  Re-verify at chain-appropriate depth (Ethereum 12, Base/Arbitrum 8 (or L1 finality where
  cheap), BSC 15, Solana `finalized` commitment, SUI checkpoint finality) and emit
  `confirmed`. If the log is absent on re-verification, emit `reorged` and soft-delete.
  A reorg-aware cursor means rewinding: on a head whose parent hash does not match the
  stored hash for `head-1`, walk back until hashes agree and re-scan forward.
- **Dedupe at the store boundary**, not in memory only:
  `UNIQUE (chain_id, tx_hash, log_index)`. Restarts and overlapping endpoints will replay;
  that must be a no-op.
- **Never let one chain's backlog stall another.** One supervised goroutine tree per
  chain, per-chain metrics, per-chain circuit breaker.

WebSocket handling (the doorbell):

- Subscribe to `newHeads` plus `logs` filtered to the factory addresses and creation
  topics. Treat every payload as *advisory*: extract only the block number, ring the
  doorbell, discard the rest.
- **Liveness check:** if the socket delivers no `newHeads` for `3 × blockTime`, or its head
  falls more than 2 blocks behind the HTTP-derived head, mark it `DEGRADED` and
  resubscribe.
- Reconnect with exponential backoff + full jitter, 0.5 s -> 30 s cap. **After 2 drops
  inside a 10-minute window, demote that endpoint to HTTP-only for a 15-minute cooldown**
  (this is the behaviour the UI already advertises: *"auto-resubscribe on socket close,
  HTTP fallback after 2 drops"*). Track drop counts on a 24h rolling window for the
  health panel.

Non-EVM chains follow the same doctrine with different primitives:

- **Solana** — truth: `getSignaturesForAddress(programID, until: lastSignature)` paged
  backwards, then `getTransaction(..., maxSupportedTransactionVersion: 0)` for the ones
  you have not seen; gap-fill with `getBlocks(start, end)` + `getBlock`. Doorbell:
  `logsSubscribe({ mentions: [raydiumAMM, orcaWhirlpool, meteora…] })`. Parse pool
  initialisation from inner instructions, not from log text where avoidable. Commitment:
  `confirmed` for the fast path, `finalized` for confirmation.
- **SUI** — truth: `suix_queryEvents` with a persisted `{ txDigest, eventSeq }` cursor over
  `MoveEventType` filters (Cetus `CreatePoolEvent`, Turbos, etc.), ascending, paged.
  Doorbell: `suix_subscribeEvent`. Checkpoints are ordered, so the cursor is exact.

Factory events to index (EVM):

| protocol            | event                                                        |
|---------------------|--------------------------------------------------------------|
| Uniswap V2 + forks  | `PairCreated(address,address,address,uint256)` — topic0 `0x0d3648bd0f6ba80134a33ba9275ac585d9d315f0ad8355cddefde31afa28d0e9` |
| Uniswap V3 + forks  | `PoolCreated(address,address,uint24,int24,address)`           |
| Uniswap V4          | `Initialize(bytes32,address,address,uint24,int24,address)`    |
| Aerodrome/Velodrome | `PoolCreated(address,address,bool,address,uint256)`           |
| Curve               | factory `PlainPoolDeployed` / `MetaPoolDeployed`              |

Also index, per pool, `Swap`, `Mint`/`Burn` (V2) and `IncreaseLiquidity`/`Collect` (V3) —
these feed volume, unique trader counts and LP position tracking.

---

## 3. RPC endpoint pool (`internal/rpcpool`) — the load-bearing component

A registry of endpoints per chain, each with transport (`ws`/`http`), a token-bucket
limiter, a health score and a circuit breaker.

- **Health score** from a rolling window: success rate, p50/p95 latency, head lag versus
  the fleet max, error classes. Statuses map to the UI: `HEALTHY` / `DEGRADED` (slow, or
  1–3 blocks behind, or recent reconnects) / `UNREACHABLE` (breaker open).
- **Hedged requests:** send to the best endpoint; if no response in p95, fire the same
  call at the second-best and take the first valid answer. Cap in-flight hedges so a slow
  chain does not exhaust the rate budget.
- **Quorum on head:** `head = max(heads)` but ignore any endpoint more than N blocks ahead
  of the median (public endpoints occasionally serve garbage). If an endpoint lags the
  median by more than 3 blocks for 30 s, pull it from rotation.
- **Rate limiting:** per-endpoint token bucket sized from config. On HTTP 429 or
  `-32097 / "rate limit"`, honour `Retry-After`, halve the bucket rate, and recover it
  linearly over 5 minutes (AIMD). On repeated 5xx, open the breaker with a 30 s probe.
- **Request classification:** cheap (`eth_blockNumber`, `eth_call`) versus expensive
  (`eth_getLogs` over a wide range, `debug_*`) get separate budgets so log scanning cannot
  starve quotes.
- **Never log a full endpoint URL with an API key.** Mask to
  `https://eth-mainnet.g.alchemy.com/v2/••••` — the UI displays exactly this masked form.
- Expose everything the health panel needs: transport, masked URL, status, current
  latency, subscription count, uptime, 24h reconnects, head block, lag, and accept the
  admin actions `resubscribe` / `ping` / `failover`.

Ship with a sensible free-tier default set (publicnode, llamarpc, ankr, 1rpc, official
chain RPCs, Solana mainnet-beta, `fullnode.mainnet.sui.io`) and let operators and users
add their own via config and `POST /rpc/endpoints`. Assume any single one can vanish; the
system must stay green on `N-1`.

---

## 4. Enrichment pipeline

A worker pool consuming newly detected pools. Each stage is independently retryable and
writes partial results — **a pool appears in the feed as soon as it is detected, with
enrichment filling in progressively**. Never block the feed on enrichment.

1. **Token metadata** — `name`, `symbol`, `decimals`, `totalSupply` via a multicall
   (Multicall3 at `0xcA11bde05977b3631167028862bE2a173976CA11` on EVM) to keep it to one
   RPC round trip. Fall back to `bytes32` symbol for ancient tokens.
2. **Liquidity** — reserves (V2) or `slot0` + `liquidity` (V3/V4) + token prices. Price
   the quote side from a stable reference pool; compute USD TVL. Refresh on a schedule
   that decays with pool age (every block for the first 5 minutes, then 15 s, then 60 s).
3. **Volume + traders** — stream `Swap` logs into the analytics table; compute 1h volume
   and **unique buyer / seller addresses** (distinct `tx.origin` or the `to`/`recipient`
   field, whichever the DEX emits). These counts drive the headline honeypot signal, so
   count *addresses*, not swaps.
4. **Holder analytics** — holder count and top-10 concentration from `Transfer` log
   aggregation (maintain a balances table; do not call an external indexer you do not
   control).
5. **Safety probes** —
   - LP lock: is the LP token held by a known locker (Unicrypt, Team Finance, PinkLock),
     burned to `0x…dead`, or vesting? Report as `Locked 12mo` / `Unlocked` / `Burned`.
   - Mint authority: EVM — does the token expose a callable `mint` with an owner, is
     ownership renounced, is it a proxy with an upgradeable implementation? Solana — is
     `mintAuthority` / `freezeAuthority` null?
   - Buy/sell tax and honeypot: **simulate** a buy and a sell against the pending state
     with `eth_call` state overrides (fund a synthetic address via the `stateOverride`
     balance/storage fields) and compare expected versus actual output. Report as
     `"5 / 9"` percent. If the simulated sell reverts, that is a honeypot — hard fail.
   - Contract verification: check the block explorer API where a key exists; otherwise
     report `Unverified source` rather than guessing.

Cache aggressively in Redis with per-field TTLs. Enrichment for a 4-hour-old pool must
not consume the same RPC budget as a 30-second-old one.

---

## 5. Risk scoring (`internal/risk`)

Produce an integer 0–100 (higher = more dangerous) plus the eight boolean checks the UI
renders. Use the prototype's pass conditions exactly:

| check                     | passes when                         |
|---------------------------|-------------------------------------|
| Unique sellers            | `sellers > 20`                      |
| Buy / sell address ratio  | `buyers / sellers < 8`              |
| LP lock                   | status starts with `Locked` (or burned) |
| Mint authority            | revoked / renounced                 |
| Buy / sell tax            | `0 / 0`                             |
| Top 10 holders            | `< 25 %` of supply                  |
| Holders                   | `> 100`                             |
| Contract verified         | source verified                     |

Weight them into the score (honeypot simulation failure and an unlocked LP with an active
mint authority should alone push past 60), keep the weights in config so they are tunable
without a deploy, and **persist the inputs alongside the score** so a score is always
explainable and back-testable. Bands: `< 25 SAFE`, `25–59 WATCH`, `>= 60 DANGER`.

Also return the verdict sentence the UI shows, in this priority order:
ratio > 20:1 -> honeypot-pattern copy; score >= gate threshold -> "High probability of rug
or honeypot. Trading is gated."; score >= 25 -> "Mixed signals…"; else "No critical
failures detected…".

---

## 6. API surface

REST base `/api/v1`, JSON, cursor pagination (`?limit=&cursor=` ->
`{ "data": [...], "nextCursor": "..." }`). Bearer JWT (SIWE-issued) on portfolio and
watch routes. Everything else is public and CDN-cacheable for 1–2 s.

```
GET  /chains
GET  /pools?chain=&risk=&minLiq=&maxAgeMin=&sort=age|liq|vol&limit=&cursor=
GET  /pools/{chainId}/{pool}
GET  /pools/{chainId}/{pool}/risk
GET  /pools/{chainId}/{pool}/traders?window=1h
GET  /rpc/health
POST /rpc/endpoints                 { chainId, transport, url }
POST /rpc/endpoints/{id}/action     { action: "resubscribe"|"ping"|"failover" }
GET  /portfolio/funds | /portfolio/holdings | /portfolio/positions
POST /quote/swap                    { chainId, pool, side, amountIn, slippageBps }
POST /quote/zap                     { chainId, pool, amountIn, splitBps, slippageBps }
POST /quote/exit                    { chainId, positionId, pct, slippageBps }
POST /tx/build                      -> { chainId, to, data, value, gas, expiresAt }
GET|POST /watch/tokens   DELETE /watch/tokens/{id}
GET|POST /watch/topics   DELETE /watch/topics/{id}
POST /auth/nonce   POST /auth/verify
```

A pool object must carry at least:

```json
{
  "chainId": "base", "address": "0x…", "pair": "AERO / USDC",
  "dex": "Aerodrome", "model": "v2",            // v2 | clmm | stable — drives the LP UI
  "createdAt": "2026-09-12T11:02:41Z", "createdTx": "0x…",
  "state": "unconfirmed",                        // unconfirmed | confirmed | reorged
  "token0": { "address": "0x…", "symbol": "AERO", "decimals": 18 },
  "token1": { "address": "0x…", "symbol": "USDC", "decimals": 6 },
  "liquidityUsd": 1240000, "volume1hUsd": 512000, "priceChange1h": 8.4,
  "buyers1h": 1840, "sellers1h": 1207,
  "risk": { "score": 6, "band": "SAFE", "checks": [ { "label": "…", "value": "…", "ok": true } ],
            "verdict": "…" },
  "feeTierBps": 30
}
```

Send raw numbers (the frontend formats) but send token amounts as **decimal strings**
with their `decimals` — never a float.

### Quotes and transaction building

Quote responses return the leg plan the UI renders verbatim, plus economics:

```json
{ "legs": [ { "step": "1", "label": "Swap USDC → AERO", "value": "0.500 USDC" } ],
  "amountOut": "…", "minReceived": "…", "priceImpactBps": 180,
  "estGasUsd": 1.84, "route": [...], "expiresAt": "…" }
```

`/tx/build` returns an **unsigned** transaction. The backend never holds keys and never
broadcasts a user transaction — it may only broadcast if the client posts a signed raw tx
to an explicit `POST /tx/send` relay endpoint. Zap and exit are single transactions
through a router/multicall contract so they revert atomically, matching the UI copy
("Reverts entirely if any leg fails"). Enforce the `minReceived` deadline on-chain, not
just in the quote. Re-simulate before returning the payload and refuse to build a
transaction whose simulation reverts, returning the decoded revert reason.

### WebSocket `/api/v1/stream`

Client: `{ "op": "sub", "topics": [...], "filters": { "chains": [...] }, "since": 918230 }`.
Server: `{ "t": "pools.new", "seq": 918233, "ts": 1750000000123, "d": { … } }`.

Topics: `pools.new`, `pools.update` (liquidity/volume/risk/state changes), `head`
(per-chain block numbers, drives the `BLOCK +Ns` pulse), `rpc.health`, `watch.hits`,
`portfolio` (authenticated).

Requirements:

- **Monotonic `seq` per connection topic set, with a replay buffer** (last ~5 minutes in
  Redis) so a client reconnecting with `since` gets exactly what it missed. This is what
  makes the frontend's reconnect lossless.
- Ping every 20 s; drop a client that misses two pongs.
- Per-connection send buffer with a bounded queue — **drop the slowest consumers rather
  than growing memory**; on overflow send `{"t":"resync"}` and let the client refetch.
- Coalesce `pools.update` per pool on a 250 ms window so a busy pool cannot flood clients.
- Fan out via Redis pub/sub so the API tier scales horizontally while a single indexer
  instance owns each chain (leader election via a Postgres advisory lock or Redis lease —
  two indexers on one chain doubles the RPC bill for nothing).

---

## 7. Watchlists

- **Token watch:** resolve a user-supplied symbol or address to canonical token identities
  per chain (address is authoritative; a bare symbol resolves to a candidate set the user
  can narrow). On every newly detected pool, match `token0`/`token1` against active
  watches and emit `watch.hits`. Store the hit so the entry shows `"{n} new pairs"` with
  the pair list on reload.
- **Topic watch:** accept a raw 32-byte topic0 or a human signature like
  `PairCreated(address,address,address,uint256)`, keccak-hashing the latter. Add the topic
  to the chain's log filter set and count matches on a 24h rolling window, keeping the
  last match's timestamp and tx hash. **Guard this:** an unbounded user-supplied topic on a
  busy chain (`Transfer`) will blow your RPC budget — enforce a per-user topic cap, reject
  topics whose first sampled window exceeds a match-rate ceiling, and always fetch these
  in the same `getLogs` sweep as the factory topics rather than as separate calls.

---

## 8. Operational requirements

- **Backfill on cold start:** each chain resumes from its stored cursor; if the gap is
  larger than a configured threshold, backfill in bounded parallel chunks with a lower
  rate-limit budget so live ingestion is not starved, and mark backfilled pools so the
  live feed does not spam clients with hours-old pairs.
- **Metrics that matter:** `pairs_detected_total{chain}`,
  `detection_latency_seconds{chain}` (block timestamp -> client frame), `cursor_lag_blocks`,
  `rpc_requests_total{endpoint,method,status}`, `rpc_rate_limited_total`,
  `ws_reconnects_total{endpoint}`, `enrichment_queue_depth`, `reorgs_total`.
  Alert on cursor lag and on detection latency p95.
- **Graceful shutdown:** drain the enrichment queue, persist cursors, close client sockets
  with code 1001 so they reconnect cleanly to another instance.
- **Tests:** unit-test the risk scorer against fixture pools (use the prototype's twelve
  sample pools as golden cases — `GROK / WETH` should score ~12, `PEPE2 / WBNB` ~91,
  `BONKAI / SOL` ~74 with its 288:41 buyer/seller ratio); integration-test the ingester
  against a recorded RPC fixture server that injects rate limits, truncated `getLogs`
  responses, a silent-but-open WebSocket, and a reorg; verify no duplicate pools and no
  missed pools under all four.
- Dockerfile (distroless, static build), `docker-compose.yml` for Postgres + Redis +
  ClickHouse, and a `README.md` covering env vars, chain config format, and how to add a
  chain or a DEX.

---

## 9. Non-negotiables

1. **Custody:** never store, request or transmit a private key or seed phrase. No key
   material exists in this service. Transactions are built unsigned and signed client-side.
2. **Correctness over latency at the data layer:** a pool that is detected 1.5 s late is
   fine; a pool that is missed, duplicated or silently wrong is not. Every ingestion path
   must be idempotent and resumable.
3. **Never present derived data as certain.** If enrichment failed, return `null` and let
   the UI render `—`. A fabricated liquidity figure is worse than a missing one, because
   the user trades on it.
4. **Budget the RPC.** Every new RPC call goes through `rpcpool` with a declared cost
   class; no direct `ethclient` calls from business logic. Log and expose the per-endpoint
   request rate so it is obvious when a feature starts burning quota.
5. **Risk gating is advisory, not enforced server-side for exits.** The API may refuse to
   build a *buy* for a pool above the caller's threshold, but must always build sells and
   liquidity removals — a user must never be locked out of exiting a position.
