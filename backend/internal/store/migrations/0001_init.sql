-- +goose Up

-- Pools are the feed. The primary key is the natural identity; the separate
-- unique index on the creating log is what makes ingestion idempotent, so a
-- restart, a redeploy or two endpoints returning the same log are all no-ops.
CREATE TABLE IF NOT EXISTS pools (
    chain_id        TEXT        NOT NULL,
    address         TEXT        NOT NULL,
    pair            TEXT        NOT NULL DEFAULT '',
    dex             TEXT        NOT NULL DEFAULT '',
    model           TEXT        NOT NULL,
    state           TEXT        NOT NULL DEFAULT 'unconfirmed',

    created_at      TIMESTAMPTZ NOT NULL,
    created_block   BIGINT      NOT NULL,
    created_tx      TEXT        NOT NULL,
    log_index       INTEGER     NOT NULL,
    backfilled      BOOLEAN     NOT NULL DEFAULT FALSE,

    token0          TEXT,
    token1          TEXT,
    fee_bps         INTEGER,

    liquidity_usd   DOUBLE PRECISION,
    volume_1h_usd   DOUBLE PRECISION,
    price_change_1h DOUBLE PRECISION,
    buyers_1h       INTEGER,
    sellers_1h      INTEGER,

    -- Risk is stored denormalised for the feed query, and in full (with its
    -- inputs) in risk_snapshots so every score stays explainable.
    risk_score      INTEGER,
    risk_band       TEXT,
    risk_verdict    TEXT,
    risk_checks     JSONB,
    risk_scored_at  TIMESTAMPTZ,

    enriched_at     TIMESTAMPTZ,
    -- How far the per-pool swap sweep has read. Without it every enrichment
    -- pass would re-request the same hour of logs for every live pool.
    swap_cursor     BIGINT,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (chain_id, address)
);

-- The dedupe boundary. Overlapping endpoints and replayed ranges collide here
-- rather than in memory, which is the only place it can be guaranteed.
CREATE UNIQUE INDEX IF NOT EXISTS pools_creating_log_uniq
    ON pools (chain_id, created_tx, log_index);

CREATE INDEX IF NOT EXISTS pools_feed_idx
    ON pools (created_at DESC, chain_id, address) WHERE state <> 'reorged';
CREATE INDEX IF NOT EXISTS pools_chain_feed_idx
    ON pools (chain_id, created_at DESC) WHERE state <> 'reorged';
CREATE INDEX IF NOT EXISTS pools_risk_idx
    ON pools (risk_score) WHERE state <> 'reorged';
CREATE INDEX IF NOT EXISTS pools_unconfirmed_idx
    ON pools (chain_id, created_block) WHERE state = 'unconfirmed';
CREATE INDEX IF NOT EXISTS pools_token_idx ON pools (chain_id, token0, token1);
CREATE INDEX IF NOT EXISTS pools_enrich_idx ON pools (enriched_at NULLS FIRST);

CREATE TABLE IF NOT EXISTS tokens (
    chain_id     TEXT NOT NULL,
    address      TEXT NOT NULL,
    symbol       TEXT,
    name         TEXT,
    decimals     INTEGER,
    total_supply NUMERIC(78, 0),

    holders      INTEGER,
    top10_pct    DOUBLE PRECISION,
    verified     BOOLEAN,
    mint_active  BOOLEAN,
    lp_lock      TEXT,
    tax_buy_bps  INTEGER,
    tax_sell_bps INTEGER,
    honeypot     BOOLEAN,

    resolved_at  TIMESTAMPTZ,
    probed_at    TIMESTAMPTZ,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (chain_id, address)
);

-- One row per chain. This is the source of truth for where ingestion is; every
-- restart resumes from here rather than from whatever a socket happens to push.
CREATE TABLE IF NOT EXISTS ingest_cursors (
    chain_id        TEXT PRIMARY KEY,
    last_block      BIGINT      NOT NULL,
    last_block_hash TEXT,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Recent block hashes, kept so a reorg can be detected by parent-hash mismatch
-- and the cursor rewound to the last block both sides agree on.
CREATE TABLE IF NOT EXISTS block_hashes (
    chain_id    TEXT   NOT NULL,
    number      BIGINT NOT NULL,
    hash        TEXT   NOT NULL,
    parent_hash TEXT   NOT NULL,
    ts          TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (chain_id, number)
);

-- The swap firehose. Plain Postgres here keeps the stack to two services; on a
-- Timescale-enabled database run
--   SELECT create_hypertable('swaps', 'ts', chunk_time_interval => INTERVAL '1 day');
-- and the same queries keep working. ClickHouse is the other option if this
-- table outgrows a single node.
CREATE TABLE IF NOT EXISTS swaps (
    chain_id     TEXT        NOT NULL,
    pool         TEXT        NOT NULL,
    block_number BIGINT      NOT NULL,
    tx_hash      TEXT        NOT NULL,
    log_index    INTEGER     NOT NULL,
    trader       TEXT        NOT NULL,
    amount0      NUMERIC(78, 0),
    amount1      NUMERIC(78, 0),
    buyer        BOOLEAN     NOT NULL,
    usd          DOUBLE PRECISION,
    ts           TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (chain_id, tx_hash, log_index)
);

-- The trader-count query is the honeypot signal's input, so it gets its own
-- covering index: distinct traders for one pool over the last hour.
CREATE INDEX IF NOT EXISTS swaps_pool_window_idx ON swaps (chain_id, pool, ts DESC);
CREATE INDEX IF NOT EXISTS swaps_ts_idx ON swaps (ts);

CREATE TABLE IF NOT EXISTS pool_reserves (
    chain_id   TEXT NOT NULL,
    pool       TEXT NOT NULL,
    reserve0   NUMERIC(78, 0),
    reserve1   NUMERIC(78, 0),
    sqrt_price NUMERIC(78, 0),
    liquidity  NUMERIC(78, 0),
    block_number BIGINT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chain_id, pool)
);

-- Every score keeps the inputs that produced it, so a verdict can be explained
-- to a user and re-run against new weights without re-reading the chain.
CREATE TABLE IF NOT EXISTS risk_snapshots (
    chain_id   TEXT        NOT NULL,
    pool       TEXT        NOT NULL,
    scored_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    score      INTEGER     NOT NULL,
    band       TEXT        NOT NULL,
    verdict    TEXT        NOT NULL,
    checks     JSONB       NOT NULL,
    inputs     JSONB       NOT NULL,
    weights    JSONB       NOT NULL,
    PRIMARY KEY (chain_id, pool, scored_at)
);

CREATE TABLE IF NOT EXISTS watch_tokens (
    id         BIGSERIAL PRIMARY KEY,
    owner      TEXT        NOT NULL,
    name       TEXT        NOT NULL,
    address    TEXT,
    chains     TEXT[]      NOT NULL DEFAULT '{}',
    resolved   BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS watch_tokens_owner_idx ON watch_tokens (owner);
CREATE INDEX IF NOT EXISTS watch_tokens_addr_idx ON watch_tokens (address) WHERE address IS NOT NULL;
CREATE INDEX IF NOT EXISTS watch_tokens_name_idx ON watch_tokens (upper(name));

CREATE TABLE IF NOT EXISTS watch_token_hits (
    watch_id   BIGINT      NOT NULL REFERENCES watch_tokens (id) ON DELETE CASCADE,
    chain_id   TEXT        NOT NULL,
    pool       TEXT        NOT NULL,
    seen_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (watch_id, chain_id, pool)
);

CREATE TABLE IF NOT EXISTS watch_topics (
    id            BIGSERIAL PRIMARY KEY,
    owner         TEXT        NOT NULL,
    name          TEXT        NOT NULL,
    signature     TEXT        NOT NULL DEFAULT '',
    topic0        TEXT        NOT NULL,
    chains        TEXT[]      NOT NULL DEFAULT '{}',
    alerts        BOOLEAN     NOT NULL DEFAULT FALSE,
    -- Sampling state for the match-rate ceiling: a user-supplied topic like
    -- Transfer would otherwise consume the entire log budget for a chain.
    sampled_at    TIMESTAMPTZ,
    sampled_rate  DOUBLE PRECISION,
    suspended     BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS watch_topics_owner_idx ON watch_topics (owner);
CREATE INDEX IF NOT EXISTS watch_topics_topic_idx ON watch_topics (topic0) WHERE NOT suspended;

CREATE TABLE IF NOT EXISTS topic_matches (
    topic0       TEXT        NOT NULL,
    chain_id     TEXT        NOT NULL,
    tx_hash      TEXT        NOT NULL,
    log_index    INTEGER     NOT NULL,
    block_number BIGINT      NOT NULL,
    ts           TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (topic0, chain_id, tx_hash, log_index)
);
CREATE INDEX IF NOT EXISTS topic_matches_window_idx ON topic_matches (topic0, ts DESC);

-- Endpoints added at runtime through POST /rpc/endpoints. Config-file endpoints
-- are not stored here; this is only for what operators and users add live.
CREATE TABLE IF NOT EXISTS rpc_endpoints (
    id         TEXT PRIMARY KEY,
    chain_id   TEXT        NOT NULL,
    transport  TEXT        NOT NULL,
    url        TEXT        NOT NULL,
    rate_limit DOUBLE PRECISION NOT NULL DEFAULT 5,
    added_by   TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Native-token prices, derived by the enricher from each chain's reference
-- pool. The service carries no external price feed, so every figure it shows is
-- traceable to a chain it already reads.
CREATE TABLE IF NOT EXISTS chain_prices (
    chain_id   TEXT PRIMARY KEY,
    native_usd DOUBLE PRECISION NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- SIWE nonces. Single-use and short-lived; the cleanup is a DELETE on expiry.
CREATE TABLE IF NOT EXISTS auth_nonces (
    nonce      TEXT PRIMARY KEY,
    address    TEXT,
    issued_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    used       BOOLEAN     NOT NULL DEFAULT FALSE
);
CREATE INDEX IF NOT EXISTS auth_nonces_expiry_idx ON auth_nonces (expires_at);

-- +goose Down
DROP TABLE IF EXISTS auth_nonces;
DROP TABLE IF EXISTS chain_prices;
DROP TABLE IF EXISTS rpc_endpoints;
DROP TABLE IF EXISTS topic_matches;
DROP TABLE IF EXISTS watch_topics;
DROP TABLE IF EXISTS watch_token_hits;
DROP TABLE IF EXISTS watch_tokens;
DROP TABLE IF EXISTS risk_snapshots;
DROP TABLE IF EXISTS pool_reserves;
DROP TABLE IF EXISTS swaps;
DROP TABLE IF EXISTS block_hashes;
DROP TABLE IF EXISTS ingest_cursors;
DROP TABLE IF EXISTS tokens;
DROP TABLE IF EXISTS pools;
