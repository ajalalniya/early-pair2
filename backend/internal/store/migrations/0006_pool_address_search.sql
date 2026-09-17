-- +goose Up

-- Pool search by address, full or by prefix, across every chain. The primary
-- key leads with chain_id, so it cannot serve a prefix match on address alone;
-- text_pattern_ops lets LIKE 'prefix%' use this index under any collation.
CREATE INDEX IF NOT EXISTS pools_address_prefix_idx ON pools (address text_pattern_ops);

-- +goose Down
DROP INDEX IF EXISTS pools_address_prefix_idx;
