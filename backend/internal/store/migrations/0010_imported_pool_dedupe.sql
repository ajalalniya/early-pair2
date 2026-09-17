-- +goose Up

-- The dedupe index exists to absorb overlapping sweeps: two reads of the same
-- creation log must collapse into one pool. A pool added by address has no
-- creation log — public endpoints prune the state needed to find it — so it is
-- stored with an empty creating tx, and several of them would collide here on
-- ('' , 0) despite being different pools. Restricting the index to pools that
-- came from a log keeps the guarantee where it means something; added pools are
-- already unique by (chain_id, address), the table's primary key.
DROP INDEX IF EXISTS pools_creating_log_uniq;
CREATE UNIQUE INDEX IF NOT EXISTS pools_creating_log_uniq
    ON pools (chain_id, created_tx, log_index) WHERE created_tx <> '';

-- +goose Down
DROP INDEX IF EXISTS pools_creating_log_uniq;
CREATE UNIQUE INDEX IF NOT EXISTS pools_creating_log_uniq
    ON pools (chain_id, created_tx, log_index);
