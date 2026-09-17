package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/earlypair/backend/internal/chain"
)

// TokenWatch is one entry in a user's token watchlist.
type TokenWatch struct {
	ID        int64           `json:"id"`
	Owner     string          `json:"-"`
	Name      string          `json:"name"`
	Address   string          `json:"address"`
	Chains    []string        `json:"chains"`
	Resolved  bool            `json:"resolved"`
	CreatedAt time.Time       `json:"createdAt"`
	Hits      []TokenWatchHit `json:"hits"`
}

// TokenWatchHit is a pool that matched a watch, stored so the entry still shows
// its hits after a page reload rather than only on the live stream.
type TokenWatchHit struct {
	ChainID chain.ID  `json:"chainId"`
	Pool    string    `json:"pool"`
	SeenAt  time.Time `json:"seenAt"`
}

func (s *Store) AddTokenWatch(ctx context.Context, owner, name, address string, chains []string) (TokenWatch, error) {
	if chains == nil {
		chains = []string{}
	}
	w := TokenWatch{Owner: owner, Name: name, Address: strings.ToLower(address), Chains: chains}
	w.Resolved = w.Address != ""
	err := s.db.QueryRow(ctx, `
INSERT INTO watch_tokens (owner, name, address, chains, resolved)
VALUES ($1,$2,$3,$4,$5)
RETURNING id, created_at`,
		owner, name, nullString(w.Address), chains, w.Resolved).Scan(&w.ID, &w.CreatedAt)
	if err != nil {
		return TokenWatch{}, fmt.Errorf("store: add token watch: %w", err)
	}
	return w, nil
}

// ResolveTokenWatch fills in the address once the symbol has been matched to a
// real token, flipping the entry out of its "resolving symbol…" state.
func (s *Store) ResolveTokenWatch(ctx context.Context, id int64, address string) error {
	_, err := s.db.Exec(ctx,
		`UPDATE watch_tokens SET address = $2, resolved = TRUE WHERE id = $1`,
		id, strings.ToLower(address))
	if err != nil {
		return fmt.Errorf("store: resolve token watch: %w", err)
	}
	return nil
}

func (s *Store) ListTokenWatches(ctx context.Context, owner string) ([]TokenWatch, error) {
	rows, err := s.db.Query(ctx, `
SELECT id, name, COALESCE(address, ''), chains, resolved, created_at
FROM watch_tokens WHERE owner = $1 ORDER BY created_at DESC`, owner)
	if err != nil {
		return nil, fmt.Errorf("store: list token watches: %w", err)
	}
	defer rows.Close()

	var out []TokenWatch
	ids := []int64{}
	for rows.Next() {
		var w TokenWatch
		if err := rows.Scan(&w.ID, &w.Name, &w.Address, &w.Chains, &w.Resolved, &w.CreatedAt); err != nil {
			return nil, err
		}
		w.Hits = []TokenWatchHit{}
		out = append(out, w)
		ids = append(ids, w.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	hitRows, err := s.db.Query(ctx, `
SELECT watch_id, chain_id, pool, seen_at
FROM watch_token_hits WHERE watch_id = ANY($1) ORDER BY seen_at DESC`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: list watch hits: %w", err)
	}
	defer hitRows.Close()

	byID := map[int64]*TokenWatch{}
	for i := range out {
		byID[out[i].ID] = &out[i]
	}
	for hitRows.Next() {
		var (
			id      int64
			chainID string
			h       TokenWatchHit
		)
		if err := hitRows.Scan(&id, &chainID, &h.Pool, &h.SeenAt); err != nil {
			return nil, err
		}
		h.ChainID = chain.ID(chainID)
		if w, ok := byID[id]; ok {
			w.Hits = append(w.Hits, h)
		}
	}
	return out, hitRows.Err()
}

func (s *Store) DeleteTokenWatch(ctx context.Context, owner string, id int64) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM watch_tokens WHERE id = $1 AND owner = $2`, id, owner)
	if err != nil {
		return fmt.Errorf("store: delete token watch: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ActiveTokenWatches returns every resolved watch across all users, for the
// matcher that runs on each newly detected pool.
func (s *Store) ActiveTokenWatches(ctx context.Context) ([]TokenWatch, error) {
	rows, err := s.db.Query(ctx, `
SELECT id, owner, name, COALESCE(address, ''), chains
FROM watch_tokens WHERE resolved AND address IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("store: active token watches: %w", err)
	}
	defer rows.Close()

	var out []TokenWatch
	for rows.Next() {
		var w TokenWatch
		if err := rows.Scan(&w.ID, &w.Owner, &w.Name, &w.Address, &w.Chains); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// RecordTokenWatchHit stores a match. The conflict clause means re-processing a
// pool never produces a duplicate alert.
func (s *Store) RecordTokenWatchHit(ctx context.Context, watchID int64, chainID chain.ID, pool string) (bool, error) {
	var seen time.Time
	err := s.db.QueryRow(ctx, `
INSERT INTO watch_token_hits (watch_id, chain_id, pool)
VALUES ($1,$2,$3)
ON CONFLICT DO NOTHING
RETURNING seen_at`, watchID, string(chainID), strings.ToLower(pool)).Scan(&seen)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: record watch hit: %w", err)
	}
	return true, nil
}

// TopicWatch is an event-signature watch.
type TopicWatch struct {
	ID          int64      `json:"id"`
	Owner       string     `json:"-"`
	Name        string     `json:"name"`
	Signature   string     `json:"signature"`
	Topic0      string     `json:"topic0"`
	Chains      []string   `json:"chains"`
	Alerts      bool       `json:"alerts"`
	Suspended   bool       `json:"suspended"`
	CreatedAt   time.Time  `json:"createdAt"`
	Matches24h  int        `json:"matches24h"`
	LastMatchAt *time.Time `json:"lastMatchAt"`
	LastTx      string     `json:"lastTx"`
}

// CountTopicWatches enforces the per-user cap. An unbounded topic on a busy
// chain is the fastest way to exhaust a public RPC budget, so the limit is a
// hard product constraint rather than a nicety.
func (s *Store) CountTopicWatches(ctx context.Context, owner string) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM watch_topics WHERE owner = $1`, owner).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count topic watches: %w", err)
	}
	return n, nil
}

func (s *Store) AddTopicWatch(ctx context.Context, w TopicWatch) (TopicWatch, error) {
	if w.Chains == nil {
		w.Chains = []string{}
	}
	err := s.db.QueryRow(ctx, `
INSERT INTO watch_topics (owner, name, signature, topic0, chains, alerts)
VALUES ($1,$2,$3,$4,$5,$6)
RETURNING id, created_at`,
		w.Owner, w.Name, w.Signature, strings.ToLower(w.Topic0), w.Chains, w.Alerts).
		Scan(&w.ID, &w.CreatedAt)
	if err != nil {
		return TopicWatch{}, fmt.Errorf("store: add topic watch: %w", err)
	}
	return w, nil
}

func (s *Store) ListTopicWatches(ctx context.Context, owner string) ([]TopicWatch, error) {
	rows, err := s.db.Query(ctx, `
SELECT w.id, w.name, w.signature, w.topic0, w.chains, w.alerts, w.suspended, w.created_at,
       COALESCE(m.cnt, 0), m.last_ts, COALESCE(m.last_tx, '')
FROM watch_topics w
LEFT JOIN LATERAL (
    SELECT COUNT(*) AS cnt,
           MAX(ts) AS last_ts,
           (SELECT tx_hash FROM topic_matches x
             WHERE x.topic0 = w.topic0 ORDER BY ts DESC LIMIT 1) AS last_tx
    FROM topic_matches t
    WHERE t.topic0 = w.topic0 AND t.ts >= now() - interval '24 hours'
) m ON TRUE
WHERE w.owner = $1
ORDER BY w.created_at DESC`, owner)
	if err != nil {
		return nil, fmt.Errorf("store: list topic watches: %w", err)
	}
	defer rows.Close()

	var out []TopicWatch
	for rows.Next() {
		var w TopicWatch
		if err := rows.Scan(&w.ID, &w.Name, &w.Signature, &w.Topic0, &w.Chains, &w.Alerts,
			&w.Suspended, &w.CreatedAt, &w.Matches24h, &w.LastMatchAt, &w.LastTx); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) DeleteTopicWatch(ctx context.Context, owner string, id int64) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM watch_topics WHERE id = $1 AND owner = $2`, id, owner)
	if err != nil {
		return fmt.Errorf("store: delete topic watch: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ActiveTopics returns the distinct topic0 values to fold into a chain's log
// sweep. They ride along with the factory topics in the same getLogs call —
// never as separate requests, which would multiply the cost per user.
func (s *Store) ActiveTopics(ctx context.Context, chainID chain.ID) ([]string, error) {
	rows, err := s.db.Query(ctx, `
SELECT DISTINCT topic0 FROM watch_topics
WHERE NOT suspended AND (cardinality(chains) = 0 OR $1 = ANY(chains))`, string(chainID))
	if err != nil {
		return nil, fmt.Errorf("store: active topics: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TopicMatch is one observed log matching a watched topic.
type TopicMatch struct {
	Topic0      string
	ChainID     chain.ID
	TxHash      string
	LogIndex    uint
	BlockNumber uint64
	Time        time.Time
}

func (s *Store) InsertTopicMatches(ctx context.Context, matches []TopicMatch) error {
	if len(matches) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, m := range matches {
		batch.Queue(`
INSERT INTO topic_matches (topic0, chain_id, tx_hash, log_index, block_number, ts)
VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`,
			strings.ToLower(m.Topic0), string(m.ChainID), strings.ToLower(m.TxHash),
			m.LogIndex, m.BlockNumber, m.Time)
	}
	br := s.db.SendBatch(ctx, batch)
	defer br.Close()
	for range matches {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("store: insert topic matches: %w", err)
		}
	}
	return nil
}

// SuspendNoisyTopics disables watches whose match rate blows past the ceiling.
// This is the guard that stops someone watching Transfer from consuming the
// entire log budget for a chain: the watch is suspended, not silently dropped,
// so the user can see what happened.
func (s *Store) SuspendNoisyTopics(ctx context.Context, maxPerHour int) ([]int64, error) {
	rows, err := s.db.Query(ctx, `
UPDATE watch_topics w SET suspended = TRUE, sampled_at = now(),
       sampled_rate = sub.rate
FROM (
  SELECT topic0, COUNT(*)::float8 AS rate
  FROM topic_matches
  WHERE ts >= now() - interval '1 hour'
  GROUP BY topic0
) sub
WHERE w.topic0 = sub.topic0 AND NOT w.suspended AND sub.rate > $1
RETURNING w.id`, maxPerHour)
	if err != nil {
		return nil, fmt.Errorf("store: suspend noisy topics: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// PruneTopicMatches keeps the match table to the 24h window the UI reports on.
func (s *Store) PruneTopicMatches(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `DELETE FROM topic_matches WHERE ts < now() - interval '48 hours'`)
	if err != nil {
		return fmt.Errorf("store: prune topic matches: %w", err)
	}
	return nil
}
