-- +goose Up

-- A Uniswap v4 pool's hook contract. A hook runs arbitrary code on every swap
-- and can take fees or refuse sells, none of which is visible from the token
-- contract, so the risk summary needs to know it is there. NULL for pools with
-- no hook and for every earlier AMM generation.
ALTER TABLE pools ADD COLUMN IF NOT EXISTS hooks TEXT;

-- +goose Down
ALTER TABLE pools DROP COLUMN IF EXISTS hooks;
