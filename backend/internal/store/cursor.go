package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/earlypair/backend/internal/chain"
)

// Cursor is the persisted ingestion position for a chain. It is the single
// reason a crash, a deploy or a network partition costs latency instead of
// data: on restart the poller resumes from here and re-reads whatever it is
// unsure about, and the pool dedupe index absorbs the overlap.
type Cursor struct {
	LastBlock uint64
	LastHash  string
}

// GetCursor returns the stored cursor, or ok=false when the chain is cold.
func (s *Store) GetCursor(ctx context.Context, chainID chain.ID) (Cursor, bool, error) {
	var c Cursor
	var hash *string
	err := s.db.QueryRow(ctx,
		`SELECT last_block, last_block_hash FROM ingest_cursors WHERE chain_id = $1`,
		string(chainID)).Scan(&c.LastBlock, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Cursor{}, false, nil
	}
	if err != nil {
		return Cursor{}, false, fmt.Errorf("store: get cursor: %w", err)
	}
	if hash != nil {
		c.LastHash = *hash
	}
	return c, true, nil
}

// SetCursor advances (or rewinds) the persisted position.
//
// This is deliberately a plain write rather than a monotonic max: a reorg
// rewind has to be able to move the cursor backwards, and refusing that would
// leave the chain permanently past the divergence point.
func (s *Store) SetCursor(ctx context.Context, chainID chain.ID, c Cursor) error {
	_, err := s.db.Exec(ctx, `
INSERT INTO ingest_cursors (chain_id, last_block, last_block_hash, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (chain_id) DO UPDATE
SET last_block = EXCLUDED.last_block,
    last_block_hash = EXCLUDED.last_block_hash,
    updated_at = now()`,
		string(chainID), c.LastBlock, nullString(c.LastHash))
	if err != nil {
		return fmt.Errorf("store: set cursor: %w", err)
	}
	return nil
}

// PutBlockHash records a block's identity so a later parent-hash mismatch can
// be detected and the divergence point found by walking back.
func (s *Store) PutBlockHash(ctx context.Context, chainID chain.ID, h Block) error {
	_, err := s.db.Exec(ctx, `
INSERT INTO block_hashes (chain_id, number, hash, parent_hash, ts)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (chain_id, number) DO UPDATE
SET hash = EXCLUDED.hash, parent_hash = EXCLUDED.parent_hash, ts = EXCLUDED.ts`,
		string(chainID), h.Number, h.Hash, h.ParentHash, h.Time)
	if err != nil {
		return fmt.Errorf("store: put block hash: %w", err)
	}
	return nil
}

// Block is a stored header identity.
type Block struct {
	Number     uint64
	Hash       string
	ParentHash string
	Time       time.Time
}

// GetBlockHash returns the stored hash for a block number.
func (s *Store) GetBlockHash(ctx context.Context, chainID chain.ID, number uint64) (string, bool, error) {
	var hash string
	err := s.db.QueryRow(ctx,
		`SELECT hash FROM block_hashes WHERE chain_id = $1 AND number = $2`,
		string(chainID), number).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: get block hash: %w", err)
	}
	return hash, true, nil
}

// DeleteBlockHashesFrom drops the orphaned side of a reorg.
func (s *Store) DeleteBlockHashesFrom(ctx context.Context, chainID chain.ID, fromBlock uint64) error {
	_, err := s.db.Exec(ctx,
		`DELETE FROM block_hashes WHERE chain_id = $1 AND number >= $2`,
		string(chainID), fromBlock)
	if err != nil {
		return fmt.Errorf("store: delete block hashes: %w", err)
	}
	return nil
}

// PruneBlockHashes keeps the reorg-detection table bounded. Anything deeper
// than the confirmation depth can never be rewound to, so it is dead weight.
func (s *Store) PruneBlockHashes(ctx context.Context, chainID chain.ID, keepFrom uint64) error {
	_, err := s.db.Exec(ctx,
		`DELETE FROM block_hashes WHERE chain_id = $1 AND number < $2`,
		string(chainID), keepFrom)
	if err != nil {
		return fmt.Errorf("store: prune block hashes: %w", err)
	}
	return nil
}
