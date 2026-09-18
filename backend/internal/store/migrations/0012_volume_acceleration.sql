-- +goose Up

-- How the last minute's volume compares with the pair's own average minute so
-- far: 3.2 means the last minute did 3.2x its usual pace. A ratio, not an
-- amount, so it reads the same on a pair doing $200 a minute and one doing
-- $200,000. NULL until the pair has enough history to say.
ALTER TABLE pools ADD COLUMN IF NOT EXISTS vol_accel DOUBLE PRECISION;

-- +goose Down
ALTER TABLE pools DROP COLUMN IF EXISTS vol_accel;
