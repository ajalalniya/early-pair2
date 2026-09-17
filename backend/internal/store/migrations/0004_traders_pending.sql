-- +goose Up

-- How many of a pair's swaps still have no known signer. Buyers and sellers are
-- distinct signers, looked up one request per transaction; on a busy chain with
-- one public endpoint the lookups trail the swaps, and the counts are a floor
-- until this reaches zero. NULL for pairs the tracker never watched.
ALTER TABLE pools ADD COLUMN IF NOT EXISTS traders_pending INT;

-- +goose Down
ALTER TABLE pools DROP COLUMN IF EXISTS traders_pending;
