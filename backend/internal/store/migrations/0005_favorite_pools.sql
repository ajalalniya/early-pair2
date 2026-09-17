-- +goose Up

-- Pairs starred in the UI. The market tracker watches a favorite for as long as
-- it stays here, past the window that ends tracking for every other pair, and
-- screening never removes one. Deleting the pool (a purged reorg) removes it.
CREATE TABLE IF NOT EXISTS favorite_pools (
    chain_id   TEXT        NOT NULL,
    address    TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chain_id, address),
    FOREIGN KEY (chain_id, address) REFERENCES pools (chain_id, address) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE IF EXISTS favorite_pools;
