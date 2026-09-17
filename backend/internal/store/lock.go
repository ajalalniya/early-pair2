package store

import (
	"context"
	"fmt"
	"hash/fnv"

	"github.com/jackc/pgx/v5"

	"github.com/earlypair/backend/internal/chain"
)

// TryChainLock takes a Postgres session advisory lock for a chain.
//
// This is leader election with no extra infrastructure: the lock is held for as
// long as the connection lives and is released automatically if the process
// dies, so a crashed indexer does not leave a chain permanently orphaned. It is
// what stops two indexers from discovering the same pools twice — which on free
// public endpoints is not merely wasteful but the difference between staying
// inside a rate budget and being throttled out of it.
//
// The returned release function must be called when the caller is done with the
// chain; it returns the dedicated connection to the pool.
func (s *Store) TryChainLock(ctx context.Context, chainID chain.ID) (bool, func(), error) {
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("store: acquire lock connection: %w", err)
	}

	key := advisoryKey(string(chainID))

	var acquired bool
	// pg_try_advisory_lock does not block: a second instance is told "no" at
	// once rather than hanging until the first one exits.
	err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&acquired)
	if err != nil {
		conn.Release()
		return false, nil, fmt.Errorf("store: chain lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return false, nil, nil
	}

	release := func() {
		// Best effort: if this fails the session is ending anyway, and Postgres
		// drops session locks when the connection closes.
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
		conn.Release()
	}
	return true, release, nil
}

// advisoryKey hashes a chain id into the int64 space advisory locks use.
func advisoryKey(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("earlypair:chain:"))
	_, _ = h.Write([]byte(s))
	// Mask the sign bit so the value is stable and positive across platforms.
	return int64(h.Sum64() & 0x7fffffffffffffff)
}

var _ pgx.Rows // keep the pgx import meaningful if the file is trimmed later
