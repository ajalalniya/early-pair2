-- +goose Up

-- The market tracker screens each new pair a few minutes after creation.
-- screened_at records that the decision was made, so a restart does not
-- re-judge a surviving pair on later figures. removed_at hides a pair that
-- failed: it leaves the feed and enrichment, but the row stays, so the dedupe
-- index still recognises its creating log and anyone holding it can still exit.
ALTER TABLE pools ADD COLUMN IF NOT EXISTS screened_at TIMESTAMPTZ;
ALTER TABLE pools ADD COLUMN IF NOT EXISTS removed_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE pools DROP COLUMN IF EXISTS removed_at;
ALTER TABLE pools DROP COLUMN IF EXISTS screened_at;
