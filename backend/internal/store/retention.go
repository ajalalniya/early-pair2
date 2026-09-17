package store

import (
	"context"
	"fmt"
	"time"
)

// Retention.
//
// The feed is about pairs launched in the last minutes, and a chain like Arc
// launches thousands a day, so the pools table grows without bound unless old
// pairs are dropped. What is kept regardless of age: pairs someone starred —
// which includes every pool added by address — and pairs someone traded, since
// the portfolio reads a holding's pool to price it.

// retentionBatch bounds one delete, so a large backlog is cleared in short
// transactions rather than one long lock on the tables the feed reads.
const retentionBatch = 5000

// PruneOldPools deletes pools created before olderThan ago, with the rows that
// hang off them and carry no foreign key: reserves, risk history and swaps. It
// deletes at most maxBatches batches per call and reports how many pools went.
//
// Only one caller works at a time, across every API replica: two overlapping
// deletes would take the same row locks in different orders and deadlock.
func (s *Store) PruneOldPools(ctx context.Context, olderThan time.Duration, maxBatches int) (int64, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-olderThan)
	var total int64
	for range maxBatches {
		n, locked, err := s.pruneOldPoolsBatch(ctx, cutoff)
		if err != nil {
			return total, err
		}
		if !locked {
			return total, nil // another replica is pruning
		}
		total += n
		if n < retentionBatch {
			return total, nil
		}
	}
	return total, nil
}

func (s *Store) pruneOldPoolsBatch(ctx context.Context, cutoff time.Time) (int64, bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("store: prune pools: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, advisoryKey("pool-retention")).Scan(&locked); err != nil {
		return 0, false, fmt.Errorf("store: prune pools: %w", err)
	}
	if !locked {
		return 0, false, nil
	}

	// One statement, so the dependent rows and the pools go together or not at
	// all. The CTE is materialised once, so every DELETE sees the same pools.
	tag, err := tx.Exec(ctx, `
WITH doomed AS MATERIALIZED (
    SELECT p.chain_id, p.address
    FROM pools p
    WHERE p.created_at < $1
      -- Reorged pools have their own, shorter purge; leaving them out also
      -- lets this use the feed's age index, which excludes them.
      AND p.state <> 'reorged'
      AND NOT EXISTS (SELECT 1 FROM favorite_pools f
                      WHERE f.chain_id = p.chain_id AND f.address = p.address)
      AND NOT EXISTS (SELECT 1 FROM portfolio_trades t
                      WHERE t.chain_id = p.chain_id AND t.pool = p.address)
    LIMIT $2
),
gone_reserves AS (
    DELETE FROM pool_reserves r USING doomed d
    WHERE r.chain_id = d.chain_id AND r.pool = d.address
),
gone_risk AS (
    DELETE FROM risk_snapshots rs USING doomed d
    WHERE rs.chain_id = d.chain_id AND rs.pool = d.address
),
gone_swaps AS (
    DELETE FROM swaps w USING doomed d
    WHERE w.chain_id = d.chain_id AND w.pool = d.address
)
DELETE FROM pools p USING doomed d
WHERE p.chain_id = d.chain_id AND p.address = d.address`, cutoff, retentionBatch)
	if err != nil {
		return 0, true, fmt.Errorf("store: prune pools: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, true, fmt.Errorf("store: prune pools: %w", err)
	}
	return tag.RowsAffected(), true, nil
}

// PruneOrphanTokens deletes token records no pool refers to any more and no
// trade was made in. A token touched within olderThan is kept even when
// unreferenced: a pool can be stored a moment before its tokens are, and its
// enrichment would otherwise race the delete.
func (s *Store) PruneOrphanTokens(ctx context.Context, olderThan time.Duration, maxBatches int) (int64, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-olderThan)
	var total int64
	for range maxBatches {
		tag, err := s.db.Exec(ctx, `
DELETE FROM tokens
WHERE ctid IN (
    SELECT t.ctid FROM tokens t
    WHERE t.updated_at < $1
      AND NOT EXISTS (SELECT 1 FROM pools p WHERE p.chain_id = t.chain_id AND p.token0 = t.address)
      AND NOT EXISTS (SELECT 1 FROM pools p WHERE p.chain_id = t.chain_id AND p.token1 = t.address)
      AND NOT EXISTS (SELECT 1 FROM portfolio_trades tr WHERE tr.chain_id = t.chain_id AND tr.token = t.address)
    LIMIT $2
)`, cutoff, retentionBatch)
		if err != nil {
			return total, fmt.Errorf("store: prune tokens: %w", err)
		}
		n := tag.RowsAffected()
		total += n
		if n < retentionBatch {
			break
		}
	}
	return total, nil
}
