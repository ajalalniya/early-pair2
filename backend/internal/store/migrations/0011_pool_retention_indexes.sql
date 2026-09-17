-- +goose Up

-- Pool retention looks up, for each token, whether any pool still uses it in
-- either slot. pools_token_idx leads with token0, so the token1 side needs an
-- index of its own or every candidate token scans the whole pools table.
CREATE INDEX IF NOT EXISTS pools_token1_idx ON pools (chain_id, token1);

-- Retention also checks whether a pool was traded. The existing index serves
-- the portfolio's by-wallet reads, not this by-pool lookup.
CREATE INDEX IF NOT EXISTS portfolio_trades_pool_idx ON portfolio_trades (chain_id, pool);

-- +goose Down
DROP INDEX IF EXISTS portfolio_trades_pool_idx;
DROP INDEX IF EXISTS pools_token1_idx;
