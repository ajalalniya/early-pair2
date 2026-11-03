# EarlyPair

EarlyPair is a terminal for finding and trading newly created DEX liquidity
pools. It spots a new pool within seconds of its creation, scores it for
signs of fraud, and lets you trade it or add liquidity from your own wallet.

It is **non-custodial**: the backend only builds unsigned transactions, and
your wallet signs them. EarlyPair never asks for, stores or sends a private
key or seed phrase.

## What it does

- **New-pool feed.** It watches pool-creation events on Uniswap v2, v3 and v4
  and their forks (Aerodrome, SushiSwap, PancakeSwap, Camelot). New pools are
  pushed to the browser live over WebSocket. Pools with too little liquidity
  or volume are dropped after a 3-minute screen.
- **Fraud scoring.** Each pool gets a 0–100 risk score from eight checks:
  unique sellers, buy/sell address ratio, LP lock, mint authority, buy/sell
  tax, top-10 holder share, holder count and whether the contract source is
  verified. You can set the app to block buys above a chosen risk level.
- **Live market data.** It tracks liquidity, rolling 1-hour volume and its
  acceleration, buyers and sellers, and price for each pool.
- **Trading.** Buy and sell with live quotes, and pay in the native coin or a
  stable. Every transaction is simulated with `eth_call` before you sign it.
- **Liquidity.** Zap into a pool, see where a concentrated-liquidity pool's
  liquidity sits across its price range, and estimate an hour of fees for a
  deposit between two prices.
- **Favorites and watchlist.** Star a pool to keep watching it. Add token
  watches to be alerted when a token you name gets a new pool.
- **Portfolio.** Tokens you bought through the app, with live balance, average
  entry and P&L. Trades are checked against their on-chain receipts.
- **RPC health.** It runs on free public RPC endpoints, with several per
  chain. It spreads load across them, retries on another endpoint when one is
  slow or down, and shows each endpoint's health in the header.

**Chains:** Base, Robinhood Chain and Arc are enabled by default. Ethereum,
BSC and Arbitrum are configured, and you can turn them on in
[`backend/config/earlypair.yaml`](backend/config/earlypair.yaml). Solana and
SUI are not implemented yet.

## How it is built

| Part | What it is |
|---|---|
| [`backend/`](backend/) | Go. `earlypair-indexer` finds and enriches pools; `earlypair-api` serves REST and WebSocket, quotes trades and builds transactions. Uses Postgres and Redis. |
| [`frontend/`](frontend/) | Vue 3 + TypeScript, with wagmi for wallets and wallet sign-in (SIWE). |
| [`deploy/`](deploy/) | Docker Compose stack for a server: nginx, api, indexer, Postgres, Redis. |
| `EarlyPair Standalone.html` | The design mockup the frontend was built from. |

## Getting started

Run the backend and the frontend locally:

```bash
# backend
cd backend
docker compose up -d postgres redis
export DATABASE_URL="postgres://earlypair:earlypair@localhost:5432/earlypair?sslmode=disable"
export EARLYPAIR_JWT_SECRET="$(openssl rand -hex 32)"
go run ./cmd/earlypair-indexer -config config/earlypair.yaml   # one terminal
go run ./cmd/earlypair-api     -config config/earlypair.yaml   # another

# frontend
cd frontend
pnpm install
pnpm dev        # http://localhost:5173
```

More detail:

- [backend/README.md](backend/README.md): architecture, configuration and the API
- [frontend/README.md](frontend/README.md): scripts, environment and layout
- [DEPLOY.md](DEPLOY.md): deploying with GitHub Actions and Docker Compose

## Disclaimer

New pools are where most rug pulls and honeypots happen. The risk score is a
set of heuristics, not a guarantee. Nothing here is financial advice; trade
at your own risk.
