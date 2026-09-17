-- +goose Up

-- Tokens a wallet removed from its "Tokens bought" list. The trades stay, so
-- the cost basis is intact if the token comes back: a trade recorded after
-- hidden_at brings the line back with its full history.
CREATE TABLE IF NOT EXISTS portfolio_hidden (
    wallet     TEXT        NOT NULL,
    chain_id   TEXT        NOT NULL,
    token      TEXT        NOT NULL,
    hidden_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (wallet, chain_id, token)
);

-- +goose Down
DROP TABLE IF EXISTS portfolio_hidden;
