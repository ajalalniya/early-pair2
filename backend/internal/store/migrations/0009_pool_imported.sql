-- +goose Up

-- Pools added by a user rather than discovered by the sweep. The indexer only
-- sees pools created while it is watching, so a pool from before that — or from
-- a gap — cannot be traded here at all unless it can be added by address. The
-- column marks how the pool arrived, which the UI shows as an "ADDED" tag: its
-- creation block came from a lookup, not from a log the sweep read live.
ALTER TABLE pools ADD COLUMN IF NOT EXISTS imported_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE pools DROP COLUMN IF EXISTS imported_at;
