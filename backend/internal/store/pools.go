package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/earlypair/backend/internal/chain"
)

// ErrNotFound is returned by single-row lookups.
var ErrNotFound = errors.New("store: not found")

// InsertPool writes a newly detected pool. It reports whether this call is the
// one that created the row.
//
// Both unique constraints matter: the primary key stops the same pool address
// being inserted twice, and the (chain, tx, log_index) index stops the same
// creating log being processed twice. Overlapping endpoints, a replayed range
// after a restart, and a re-scan following a reorg rewind all land here and all
// become no-ops — which is what lets the poller be aggressive about re-reading
// ranges it is not certain about.
func (s *Store) InsertPool(ctx context.Context, p chain.Pool) (created bool, err error) {
	const q = `
INSERT INTO pools (chain_id, address, pair, dex, model, state,
                   created_at, created_block, created_tx, log_index, backfilled,
                   token0, token1, fee_bps, hooks)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
ON CONFLICT DO NOTHING
RETURNING address`

	var t0, t1 any
	if p.Token0 != nil {
		t0 = strings.ToLower(p.Token0.Address)
	}
	if p.Token1 != nil {
		t1 = strings.ToLower(p.Token1.Address)
	}

	var addr string
	err = s.db.QueryRow(ctx, q,
		string(p.ChainID), strings.ToLower(p.Address), p.Pair, p.Dex, string(p.Model), string(p.State),
		p.CreatedAt, p.CreatedBlock, strings.ToLower(p.CreatedTx), p.LogIndex, p.Backfilled,
		t0, t1, p.FeeTierBps, nullString(strings.ToLower(p.Hooks)),
	).Scan(&addr)

	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // already known: the dedupe worked
	}
	if err != nil {
		return false, fmt.Errorf("store: insert pool %s: %w", p.Key(), err)
	}
	return true, nil
}

// ImportPool records a pool a user added by address. A pool the sweep already
// knows is left alone and reported as not created, so adding a listed pair is a
// no-op rather than a way to rewrite its history.
func (s *Store) ImportPool(ctx context.Context, p chain.Pool) (created bool, err error) {
	const q = `
INSERT INTO pools (chain_id, address, pair, dex, model, state,
                   created_at, created_block, created_tx, log_index, backfilled,
                   token0, token1, fee_bps, hooks, imported_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15, now())
ON CONFLICT DO NOTHING
RETURNING address`

	var t0, t1 any
	if p.Token0 != nil {
		t0 = strings.ToLower(p.Token0.Address)
	}
	if p.Token1 != nil {
		t1 = strings.ToLower(p.Token1.Address)
	}
	var addr string
	err = s.db.QueryRow(ctx, q,
		string(p.ChainID), strings.ToLower(p.Address), p.Pair, p.Dex, string(p.Model), string(p.State),
		p.CreatedAt, p.CreatedBlock, strings.ToLower(p.CreatedTx), p.LogIndex, p.Backfilled,
		t0, t1, p.FeeTierBps, nullString(strings.ToLower(p.Hooks)),
	).Scan(&addr)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: import pool %s: %w", p.Key(), err)
	}
	return true, nil
}

// SetPoolState flips a pool between unconfirmed, confirmed and reorged.
func (s *Store) SetPoolState(ctx context.Context, chainID chain.ID, address string, state chain.State) error {
	_, err := s.db.Exec(ctx,
		`UPDATE pools SET state = $3, updated_at = now() WHERE chain_id = $1 AND address = $2`,
		string(chainID), strings.ToLower(address), string(state))
	if err != nil {
		return fmt.Errorf("store: set pool state: %w", err)
	}
	return nil
}

// UnconfirmedBelow returns pools that are now deep enough to verify.
func (s *Store) UnconfirmedBelow(ctx context.Context, chainID chain.ID, block uint64, limit int) ([]chain.Pool, error) {
	rows, err := s.db.Query(ctx, selectPoolCols+poolJoins+`
WHERE p.chain_id = $1 AND p.state = 'unconfirmed' AND p.created_block <= $2
  AND p.removed_at IS NULL
ORDER BY p.created_block ASC
LIMIT $3`, string(chainID), block, limit)
	if err != nil {
		return nil, fmt.Errorf("store: unconfirmed pools: %w", err)
	}
	defer rows.Close()
	return scanPools(rows)
}

// ReorgPoolsFrom marks every pool created at or above a block as reorged. Called
// when the ingester rewinds; the re-scan re-inserts whatever genuinely survived.
func (s *Store) ReorgPoolsFrom(ctx context.Context, chainID chain.ID, fromBlock uint64) ([]string, error) {
	rows, err := s.db.Query(ctx, `
UPDATE pools SET state = 'reorged', updated_at = now()
WHERE chain_id = $1 AND created_block >= $2 AND state <> 'reorged'
RETURNING address`, string(chainID), fromBlock)
	if err != nil {
		return nil, fmt.Errorf("store: reorg pools: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RestorePool brings a reorged pool back when its creating log turns up again
// on the new canonical chain.
//
// This is how a rewind avoids announcing the same pool twice. The alternative —
// deleting the dedupe rows so the rescan re-inserts them — cannot work, because
// a conservative rewind covers blocks that never reorged at all, and every pool
// in them would arrive in the feed a second time. Restoring keeps the original
// row, so the pool is a state change, not a discovery.
func (s *Store) RestorePool(ctx context.Context, chainID chain.ID, txHash string, logIndex uint) (bool, error) {
	tag, err := s.db.Exec(ctx, `
UPDATE pools SET state = 'unconfirmed', updated_at = now()
WHERE chain_id = $1 AND created_tx = $2 AND log_index = $3 AND state = 'reorged'`,
		string(chainID), strings.ToLower(txHash), logIndex)
	if err != nil {
		return false, fmt.Errorf("store: restore pool: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// PurgeReorged removes pools that stayed reorged past the point where they
// could still come back. Called by the janitor so the feed's dedupe index does
// not accumulate rows for pools that never existed.
func (s *Store) PurgeReorged(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.db.Exec(ctx,
		`DELETE FROM pools WHERE state = 'reorged' AND updated_at < $1`,
		time.Now().Add(-olderThan))
	if err != nil {
		return 0, fmt.Errorf("store: purge reorged pools: %w", err)
	}
	return tag.RowsAffected(), nil
}

// UpdateMarket writes the enrichment results. Nil values stay nil in the
// database: "we do not know this yet" must survive to the API, because the
// alternative is a number the user would size a position against.
func (s *Store) UpdateMarket(ctx context.Context, chainID chain.ID, address string, m Market) error {
	const q = `
UPDATE pools SET
    pair            = COALESCE(NULLIF($3, ''), pair),
    liquidity_usd   = COALESCE($4, liquidity_usd),
    volume_1h_usd   = COALESCE($5, volume_1h_usd),
    price_change_1h = COALESCE($6, price_change_1h),
    buyers_1h       = COALESCE($7, buyers_1h),
    sellers_1h      = COALESCE($8, sellers_1h),
    enriched_at     = now(),
    updated_at      = now()
WHERE chain_id = $1 AND address = $2`
	_, err := s.db.Exec(ctx, q, string(chainID), strings.ToLower(address),
		m.Pair, m.LiquidityUSD, m.Volume1hUSD, m.PriceChange1h, m.Buyers1h, m.Sellers1h)
	if err != nil {
		return fmt.Errorf("store: update market: %w", err)
	}
	return nil
}

// Market is the enrichment payload. Pointers distinguish "no change" from zero.
type Market struct {
	Pair          string
	LiquidityUSD  *float64
	Volume1hUSD   *float64
	PriceChange1h *float64
	Buyers1h      *int
	Sellers1h     *int
	// TradersPending is written by the market tracker only: swaps whose signer
	// is not known yet, so Buyers1h and Sellers1h are a floor while it is above
	// zero.
	TradersPending *int
}

// UpdateRisk stores the denormalised summary on the pool and appends the full
// snapshot, inputs included, so the verdict stays explainable and back-testable.
func (s *Store) UpdateRisk(ctx context.Context, chainID chain.ID, address string, r chain.RiskSummary, inputs any, weights any) error {
	checks, err := json.Marshal(r.Checks)
	if err != nil {
		return err
	}
	inputsJSON, err := json.Marshal(inputs)
	if err != nil {
		return err
	}
	weightsJSON, err := json.Marshal(weights)
	if err != nil {
		return err
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: risk tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
UPDATE pools SET risk_score = $3, risk_band = $4, risk_verdict = $5,
                 risk_checks = $6, risk_scored_at = $7, updated_at = now()
WHERE chain_id = $1 AND address = $2`,
		string(chainID), strings.ToLower(address), r.Score, r.Band, r.Verdict, checks, r.ScoredAt,
	); err != nil {
		return fmt.Errorf("store: update risk: %w", err)
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO risk_snapshots (chain_id, pool, scored_at, score, band, verdict, checks, inputs, weights)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT DO NOTHING`,
		string(chainID), strings.ToLower(address), r.ScoredAt, r.Score, r.Band, r.Verdict,
		checks, inputsJSON, weightsJSON,
	); err != nil {
		return fmt.Errorf("store: insert risk snapshot: %w", err)
	}

	return tx.Commit(ctx)
}

// PoolFilter is the /pools query.
type PoolFilter struct {
	ChainID   chain.ID
	Risk      string // "", "safe", "watch", "danger"
	MinLiq    *float64
	MaxAgeMin *int
	Sort      string // "age" (default), "liq", "vol"
	Limit     int
	Cursor    string
}

const selectPoolCols = `
SELECT p.chain_id, p.address, p.pair, p.dex, p.model, p.state,
       p.created_at, p.created_block, p.created_tx, p.log_index, p.backfilled,
       p.token0, p.token1, p.fee_bps, COALESCE(p.hooks, ''),
       p.liquidity_usd, p.volume_1h_usd, p.price_change_1h, p.buyers_1h, p.sellers_1h,
       p.risk_score, p.risk_band, p.risk_verdict, p.risk_checks, p.risk_scored_at,
       t0.symbol, t0.decimals, t1.symbol, t1.decimals,
       p.screened_at, p.removed_at, p.traders_pending, fp.created_at, p.imported_at
`

const poolJoins = `
FROM pools p
LEFT JOIN tokens t0 ON t0.chain_id = p.chain_id AND t0.address = p.token0
LEFT JOIN tokens t1 ON t1.chain_id = p.chain_id AND t1.address = p.token1
LEFT JOIN favorite_pools fp ON fp.chain_id = p.chain_id AND fp.address = p.address
`

// ListPools runs the feed query with keyset pagination. The cursor carries the
// sort key plus the identity tiebreak, so pages stay stable while new pools are
// arriving between requests.
func (s *Store) ListPools(ctx context.Context, f PoolFilter) ([]chain.Pool, string, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	var (
		where []string
		args  []any
	)
	add := func(clause string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}

	where = append(where, "p.state <> 'reorged'", "p.removed_at IS NULL")

	if f.ChainID != "" && f.ChainID != "all" {
		add("p.chain_id = $%d", string(f.ChainID))
	}
	switch f.Risk {
	case "safe":
		where = append(where, "p.risk_score IS NOT NULL AND p.risk_score < 25")
	case "watch":
		where = append(where, "p.risk_score IS NOT NULL AND p.risk_score >= 25 AND p.risk_score < 60")
	case "danger":
		where = append(where, "p.risk_score IS NOT NULL AND p.risk_score >= 60")
	}
	if f.MinLiq != nil {
		add("p.liquidity_usd >= $%d", *f.MinLiq)
	}
	if f.MaxAgeMin != nil {
		add("p.created_at >= $%d", time.Now().Add(-time.Duration(*f.MaxAgeMin)*time.Minute))
	}

	sortCol, order := "p.created_at", "DESC"
	switch f.Sort {
	case "liq":
		sortCol = "p.liquidity_usd"
	case "vol":
		sortCol = "p.volume_1h_usd"
	}

	if f.Cursor != "" {
		cur, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", err
		}
		// Strictly-after comparison on (sortKey, chain, address). NULLS LAST is
		// implicit in DESC ordering for the numeric sorts, so a null sort key
		// only ever appears on the final page.
		if f.Sort == "liq" || f.Sort == "vol" {
			args = append(args, cur.Num, cur.Chain, cur.Address)
			where = append(where, fmt.Sprintf(
				"(COALESCE(%s, -1) , p.chain_id, p.address) < ($%d, $%d, $%d)",
				sortCol, len(args)-2, len(args)-1, len(args)))
		} else {
			args = append(args, cur.Time, cur.Chain, cur.Address)
			where = append(where, fmt.Sprintf(
				"(%s, p.chain_id, p.address) < ($%d, $%d, $%d)",
				sortCol, len(args)-2, len(args)-1, len(args)))
		}
	}

	args = append(args, limit+1)
	q := selectPoolCols + poolJoins +
		" WHERE " + strings.Join(where, " AND ") +
		fmt.Sprintf(" ORDER BY %s %s NULLS LAST, p.chain_id %s, p.address %s LIMIT $%d",
			sortCol, order, order, order, len(args))

	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("store: list pools: %w", err)
	}
	defer rows.Close()

	pools, err := scanPools(rows)
	if err != nil {
		return nil, "", err
	}

	var next string
	if len(pools) > limit {
		last := pools[limit-1]
		pools = pools[:limit]
		c := cursor{Chain: string(last.ChainID), Address: last.Address, Time: last.CreatedAt}
		switch f.Sort {
		case "liq":
			c.Num = valueOr(last.LiquidityUSD, -1)
		case "vol":
			c.Num = valueOr(last.Volume1hUSD, -1)
		}
		next = encodeCursor(c)
	}
	return pools, next, nil
}

// GetPool loads one pool with its token metadata.
func (s *Store) GetPool(ctx context.Context, chainID chain.ID, address string) (chain.Pool, error) {
	rows, err := s.db.Query(ctx, selectPoolCols+poolJoins+
		` WHERE p.chain_id = $1 AND p.address = $2`, string(chainID), strings.ToLower(address))
	if err != nil {
		return chain.Pool{}, fmt.Errorf("store: get pool: %w", err)
	}
	defer rows.Close()

	pools, err := scanPools(rows)
	if err != nil {
		return chain.Pool{}, err
	}
	if len(pools) == 0 {
		return chain.Pool{}, ErrNotFound
	}
	return pools[0], nil
}

// PoolsNeedingEnrichment returns pools whose enrichment is missing or stale.
// Freshness decays with age so a four-hour-old pool does not cost the same RPC
// budget as one that is thirty seconds old.
func (s *Store) PoolsNeedingEnrichment(ctx context.Context, limit int) ([]chain.Pool, error) {
	rows, err := s.db.Query(ctx, selectPoolCols+poolJoins+`
WHERE p.state <> 'reorged' AND p.removed_at IS NULL
  AND (
    p.enriched_at IS NULL
    OR (age(now(), p.created_at) < interval '5 minutes'  AND p.enriched_at < now() - interval '15 seconds')
    OR (age(now(), p.created_at) < interval '1 hour'     AND p.enriched_at < now() - interval '60 seconds')
    OR (age(now(), p.created_at) < interval '24 hours'   AND p.enriched_at < now() - interval '10 minutes')
    OR p.enriched_at < now() - interval '1 hour'
  )
ORDER BY p.created_at DESC
LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: pools needing enrichment: %w", err)
	}
	defer rows.Close()
	return scanPools(rows)
}

// FindPoolsByToken backs the token watchlist: every non-reorged pool that has
// the given token on either side.
func (s *Store) FindPoolsByToken(ctx context.Context, tokenAddr string, chains []string) ([]chain.Pool, error) {
	q := selectPoolCols + poolJoins + `
WHERE p.state <> 'reorged' AND (p.token0 = $1 OR p.token1 = $1)`
	args := []any{strings.ToLower(tokenAddr)}
	if len(chains) > 0 {
		args = append(args, chains)
		q += fmt.Sprintf(" AND p.chain_id = ANY($%d)", len(args))
	}
	q += " ORDER BY p.created_at DESC LIMIT 50"

	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: find pools by token: %w", err)
	}
	defer rows.Close()
	return scanPools(rows)
}

func scanPools(rows pgx.Rows) ([]chain.Pool, error) {
	var out []chain.Pool
	for rows.Next() {
		var (
			p          chain.Pool
			chainID    string
			model      string
			state      string
			t0Addr     *string
			t1Addr     *string
			riskScore  *int
			riskBand   *string
			riskVerd   *string
			riskChecks []byte
			riskAt     *time.Time
			t0Sym      *string
			t0Dec      *int
			t1Sym      *string
			t1Dec      *int
		)
		err := rows.Scan(
			&chainID, &p.Address, &p.Pair, &p.Dex, &model, &state,
			&p.CreatedAt, &p.CreatedBlock, &p.CreatedTx, &p.LogIndex, &p.Backfilled,
			&t0Addr, &t1Addr, &p.FeeTierBps, &p.Hooks,
			&p.LiquidityUSD, &p.Volume1hUSD, &p.PriceChange1h, &p.Buyers1h, &p.Sellers1h,
			&riskScore, &riskBand, &riskVerd, &riskChecks, &riskAt,
			&t0Sym, &t0Dec, &t1Sym, &t1Dec,
			&p.ScreenedAt, &p.RemovedAt, &p.TradersPending, &p.FavoritedAt, &p.ImportedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("store: scan pool: %w", err)
		}
		p.ChainID = chain.ID(chainID)
		p.Model = chain.Model(model)
		p.State = chain.State(state)

		if t0Addr != nil {
			p.Token0 = &chain.Token{Address: *t0Addr}
			if t0Sym != nil {
				p.Token0.Symbol = *t0Sym
			}
			if t0Dec != nil {
				p.Token0.Decimals = *t0Dec
			}
		}
		if t1Addr != nil {
			p.Token1 = &chain.Token{Address: *t1Addr}
			if t1Sym != nil {
				p.Token1.Symbol = *t1Sym
			}
			if t1Dec != nil {
				p.Token1.Decimals = *t1Dec
			}
		}
		if riskScore != nil {
			rs := chain.RiskSummary{Score: *riskScore}
			if riskBand != nil {
				rs.Band = *riskBand
			}
			if riskVerd != nil {
				rs.Verdict = *riskVerd
			}
			if riskAt != nil {
				rs.ScoredAt = *riskAt
			}
			if len(riskChecks) > 0 {
				_ = json.Unmarshal(riskChecks, &rs.Checks)
			}
			p.Risk = &rs
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type cursor struct {
	Chain   string    `json:"c"`
	Address string    `json:"a"`
	Time    time.Time `json:"t"`
	Num     float64   `json:"n,omitempty"`
}

func encodeCursor(c cursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, fmt.Errorf("store: bad cursor: %w", err)
	}
	var c cursor
	if err := json.Unmarshal(b, &c); err != nil {
		return cursor{}, fmt.Errorf("store: bad cursor: %w", err)
	}
	return c, nil
}

func valueOr(p *float64, def float64) float64 {
	if p == nil {
		return def
	}
	return *p
}

// TrackablePools returns the pools the market tracker should be watching on a
// chain, oldest first: pairs created since a time that were found live and are
// still in the feed, plus every favorite, whatever its age or state. Backfilled
// pools are left out — they were found after their launch window had passed —
// as are pools a reorg removed.
func (s *Store) TrackablePools(ctx context.Context, chainID chain.ID, since time.Time) ([]chain.Pool, error) {
	rows, err := s.db.Query(ctx, selectPoolCols+poolJoins+`
WHERE p.chain_id = $1 AND p.state <> 'reorged'
  AND ((NOT p.backfilled AND p.removed_at IS NULL AND p.created_at >= $2) OR fp.address IS NOT NULL)
ORDER BY p.created_block ASC`, string(chainID), since)
	if err != nil {
		return nil, fmt.Errorf("store: trackable pools: %w", err)
	}
	defer rows.Close()
	return scanPools(rows)
}

// UpdateTrackedMarket writes the market tracker's figures.
//
// It differs from UpdateMarket in one way that matters: it leaves enriched_at
// alone. The enrichment schedule keys off that column to decide which pools
// still need their token names and risk probes, and a tracker writing every
// few seconds would otherwise mark brand-new pools as freshly enriched before
// their tokens were ever resolved.
func (s *Store) UpdateTrackedMarket(ctx context.Context, chainID chain.ID, address string, m Market) error {
	_, err := s.db.Exec(ctx, `
UPDATE pools SET
    liquidity_usd   = COALESCE($3, liquidity_usd),
    volume_1h_usd   = COALESCE($4, volume_1h_usd),
    price_change_1h = COALESCE($5, price_change_1h),
    buyers_1h       = COALESCE($6, buyers_1h),
    sellers_1h      = COALESCE($7, sellers_1h),
    traders_pending = COALESCE($8, traders_pending),
    updated_at      = now()
WHERE chain_id = $1 AND address = $2`,
		string(chainID), strings.ToLower(address),
		m.LiquidityUSD, m.Volume1hUSD, m.PriceChange1h, m.Buyers1h, m.Sellers1h, m.TradersPending)
	if err != nil {
		return fmt.Errorf("store: update tracked market: %w", err)
	}
	return nil
}

// ScreenPool records the market tracker's verdict on a new pair: kept, or
// removed from the feed. It returns the time recorded.
func (s *Store) ScreenPool(ctx context.Context, chainID chain.ID, address string, keep bool) (time.Time, error) {
	var at time.Time
	err := s.db.QueryRow(ctx, `
UPDATE pools SET
    screened_at = now(),
    removed_at  = CASE WHEN $3 THEN NULL ELSE now() END,
    updated_at  = now()
WHERE chain_id = $1 AND address = $2
RETURNING screened_at`,
		string(chainID), strings.ToLower(address), keep).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("store: screen pool: %w", err)
	}
	return at, nil
}

// SearchPools finds pools on any chain whose address starts with prefix (a full
// address is its own prefix), exact matches first, then newest. Pools removed
// by screening are included — someone searching an address wants to find it —
// and carry RemovedAt; reorged pools are not, since they never existed. The
// prefix must already be validated lowercase hex: it goes into a LIKE pattern.
func (s *Store) SearchPools(ctx context.Context, prefix string, limit int) ([]chain.Pool, error) {
	prefix = strings.ToLower(prefix)
	rows, err := s.db.Query(ctx, selectPoolCols+poolJoins+`
WHERE p.state <> 'reorged' AND p.address LIKE $1
ORDER BY (p.address = $2) DESC, p.created_at DESC
LIMIT $3`, prefix+"%", prefix, limit)
	if err != nil {
		return nil, fmt.Errorf("store: search pools: %w", err)
	}
	defer rows.Close()
	return scanPools(rows)
}

// MaxFavorites caps starred pairs. Each one is watched indefinitely out of the
// same public RPC budget that discovers pools, so the list cannot be unbounded.
const MaxFavorites = 50

// ErrFavoritesFull is returned when the favorites list is at MaxFavorites.
var ErrFavoritesFull = errors.New("store: favorites list is full")

// AddFavorite stars a pool. Starring one that is already starred is a no-op.
func (s *Store) AddFavorite(ctx context.Context, chainID chain.ID, address string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: add favorite: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// Serialise concurrent stars so two requests cannot both pass the cap.
	if _, err := tx.Exec(ctx, `LOCK TABLE favorite_pools IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return fmt.Errorf("store: add favorite: %w", err)
	}
	var exists, count int
	if err := tx.QueryRow(ctx, `
SELECT (SELECT count(*) FROM pools WHERE chain_id = $1 AND address = $2),
       (SELECT count(*) FROM favorite_pools)`, string(chainID), strings.ToLower(address)).Scan(&exists, &count); err != nil {
		return fmt.Errorf("store: add favorite: %w", err)
	}
	if exists == 0 {
		return ErrNotFound
	}
	tag, err := tx.Exec(ctx, `
INSERT INTO favorite_pools (chain_id, address)
SELECT $1, $2 WHERE $3::int < $4::int
ON CONFLICT DO NOTHING`, string(chainID), strings.ToLower(address), count, MaxFavorites)
	if err != nil {
		return fmt.Errorf("store: add favorite: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var already bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM favorite_pools WHERE chain_id = $1 AND address = $2)`,
			string(chainID), strings.ToLower(address)).Scan(&already); err != nil {
			return fmt.Errorf("store: add favorite: %w", err)
		}
		if !already {
			return ErrFavoritesFull
		}
	}
	return tx.Commit(ctx)
}

// RemoveFavorite unstars a pool. Unstarring one that is not starred is a no-op.
func (s *Store) RemoveFavorite(ctx context.Context, chainID chain.ID, address string) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM favorite_pools WHERE chain_id = $1 AND address = $2`,
		string(chainID), strings.ToLower(address)); err != nil {
		return fmt.Errorf("store: remove favorite: %w", err)
	}
	return nil
}

// ListFavorites returns every starred pool, most recently starred first.
func (s *Store) ListFavorites(ctx context.Context) ([]chain.Pool, error) {
	rows, err := s.db.Query(ctx, selectPoolCols+poolJoins+`
WHERE fp.address IS NOT NULL
ORDER BY fp.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list favorites: %w", err)
	}
	defer rows.Close()
	return scanPools(rows)
}
