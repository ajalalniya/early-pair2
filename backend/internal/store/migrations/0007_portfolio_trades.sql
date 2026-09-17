-- +goose Up

-- Swaps made through EarlyPair, one row per transaction, each verified against
-- its mined receipt before it is written: the signer, the router it went
-- through, and the pool's own Swap event. Portfolio holdings and their cost
-- basis come from these rows, so a trade the service cannot verify is never
-- counted. Amounts are base-unit decimal strings.
CREATE TABLE IF NOT EXISTS portfolio_trades (
    chain_id      TEXT        NOT NULL,
    tx_hash       TEXT        NOT NULL,
    wallet        TEXT        NOT NULL,
    pool          TEXT        NOT NULL,
    token         TEXT        NOT NULL, -- the token bought or sold
    side          TEXT        NOT NULL CHECK (side IN ('buy', 'sell')),
    token_amount  TEXT        NOT NULL, -- received on a buy, sent on a sell
    quote_token   TEXT        NOT NULL,
    quote_amount  TEXT        NOT NULL, -- paid on a buy, received on a sell
    quote_usd     DOUBLE PRECISION,     -- dollar value of quote_amount when recorded; NULL if unpriced
    block_number  BIGINT      NOT NULL,
    recorded_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chain_id, tx_hash)
);

CREATE INDEX IF NOT EXISTS portfolio_trades_wallet_idx ON portfolio_trades (wallet, chain_id, token);

-- +goose Down
DROP TABLE IF EXISTS portfolio_trades;
