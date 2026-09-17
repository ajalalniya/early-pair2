package store

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/earlypair/backend/internal/chain"
)

// TokenRecord is the persisted token identity plus the safety-probe results.
// Every probe field is a pointer: "not probed yet" and "probed, came back
// false" are different facts and the risk scorer treats them differently.
type TokenRecord struct {
	ChainID     chain.ID
	Address     string
	Symbol      string
	Name        string
	Decimals    *int
	TotalSupply *big.Int

	Holders    *int
	Top10Pct   *float64
	Verified   *bool
	MintActive *bool
	LPLock     string
	TaxBuyBps  *int
	TaxSellBps *int
	Honeypot   *bool

	ResolvedAt *time.Time
	ProbedAt   *time.Time
}

// UpsertToken writes metadata without clobbering fields the caller did not
// resolve — a partial enrichment must not erase a previous full one.
func (s *Store) UpsertToken(ctx context.Context, t TokenRecord) error {
	var supply *string
	if t.TotalSupply != nil {
		str := t.TotalSupply.String()
		supply = &str
	}
	_, err := s.db.Exec(ctx, `
INSERT INTO tokens (chain_id, address, symbol, name, decimals, total_supply,
                    holders, top10_pct, verified, mint_active, lp_lock,
                    tax_buy_bps, tax_sell_bps, honeypot, resolved_at, probed_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16, now())
ON CONFLICT (chain_id, address) DO UPDATE SET
    symbol       = COALESCE(NULLIF(EXCLUDED.symbol, ''), tokens.symbol),
    name         = COALESCE(NULLIF(EXCLUDED.name, ''), tokens.name),
    decimals     = COALESCE(EXCLUDED.decimals, tokens.decimals),
    total_supply = COALESCE(EXCLUDED.total_supply, tokens.total_supply),
    holders      = COALESCE(EXCLUDED.holders, tokens.holders),
    top10_pct    = COALESCE(EXCLUDED.top10_pct, tokens.top10_pct),
    verified     = COALESCE(EXCLUDED.verified, tokens.verified),
    mint_active  = COALESCE(EXCLUDED.mint_active, tokens.mint_active),
    lp_lock      = COALESCE(NULLIF(EXCLUDED.lp_lock, ''), tokens.lp_lock),
    tax_buy_bps  = COALESCE(EXCLUDED.tax_buy_bps, tokens.tax_buy_bps),
    tax_sell_bps = COALESCE(EXCLUDED.tax_sell_bps, tokens.tax_sell_bps),
    honeypot     = COALESCE(EXCLUDED.honeypot, tokens.honeypot),
    resolved_at  = COALESCE(EXCLUDED.resolved_at, tokens.resolved_at),
    probed_at    = COALESCE(EXCLUDED.probed_at, tokens.probed_at),
    updated_at   = now()`,
		string(t.ChainID), strings.ToLower(t.Address), t.Symbol, t.Name, t.Decimals, supply,
		t.Holders, t.Top10Pct, t.Verified, t.MintActive, t.LPLock,
		t.TaxBuyBps, t.TaxSellBps, t.Honeypot, t.ResolvedAt, t.ProbedAt)
	if err != nil {
		return fmt.Errorf("store: upsert token: %w", err)
	}
	return nil
}

// GetToken loads one token record.
func (s *Store) GetToken(ctx context.Context, chainID chain.ID, address string) (TokenRecord, error) {
	var (
		t      TokenRecord
		supply *string
	)
	err := s.db.QueryRow(ctx, `
SELECT symbol, name, decimals, total_supply, holders, top10_pct, verified, mint_active,
       lp_lock, tax_buy_bps, tax_sell_bps, honeypot, resolved_at, probed_at
FROM tokens WHERE chain_id = $1 AND address = $2`,
		string(chainID), strings.ToLower(address)).Scan(
		&t.Symbol, &t.Name, &t.Decimals, &supply, &t.Holders, &t.Top10Pct, &t.Verified, &t.MintActive,
		&t.LPLock, &t.TaxBuyBps, &t.TaxSellBps, &t.Honeypot, &t.ResolvedAt, &t.ProbedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenRecord{}, ErrNotFound
	}
	if err != nil {
		return TokenRecord{}, fmt.Errorf("store: get token: %w", err)
	}
	t.ChainID = chainID
	t.Address = strings.ToLower(address)
	if supply != nil {
		n, ok := new(big.Int).SetString(*supply, 10)
		if ok {
			t.TotalSupply = n
		}
	}
	return t, nil
}

// ResolveTokenBySymbol backs the token watchlist's symbol input. A symbol is
// ambiguous by nature, so this returns every candidate and the caller decides
// (or asks the user) rather than silently picking one.
func (s *Store) ResolveTokenBySymbol(ctx context.Context, symbol string) ([]TokenRecord, error) {
	rows, err := s.db.Query(ctx, `
SELECT chain_id, address, symbol, decimals
FROM tokens WHERE upper(symbol) = upper($1)
ORDER BY chain_id LIMIT 25`, symbol)
	if err != nil {
		return nil, fmt.Errorf("store: resolve symbol: %w", err)
	}
	defer rows.Close()

	var out []TokenRecord
	for rows.Next() {
		var t TokenRecord
		var chainID string
		if err := rows.Scan(&chainID, &t.Address, &t.Symbol, &t.Decimals); err != nil {
			return nil, err
		}
		t.ChainID = chain.ID(chainID)
		out = append(out, t)
	}
	return out, rows.Err()
}

// SwapRecord is one decoded trade headed for the analytics table.
type SwapRecord struct {
	ChainID     chain.ID
	Pool        string
	BlockNumber uint64
	TxHash      string
	LogIndex    uint
	Trader      string
	Amount0     *big.Int
	Amount1     *big.Int
	Buyer       bool
	USD         *float64
	Time        time.Time
}

// InsertSwaps bulk-writes trades, ignoring ones already recorded. Same dedupe
// discipline as pools: the log's identity is the key.
func (s *Store) InsertSwaps(ctx context.Context, swaps []SwapRecord) error {
	if len(swaps) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, w := range swaps {
		var a0, a1 *string
		if w.Amount0 != nil {
			v := w.Amount0.String()
			a0 = &v
		}
		if w.Amount1 != nil {
			v := w.Amount1.String()
			a1 = &v
		}
		batch.Queue(`
INSERT INTO swaps (chain_id, pool, block_number, tx_hash, log_index, trader, amount0, amount1, buyer, usd, ts)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
ON CONFLICT (chain_id, tx_hash, log_index) DO NOTHING`,
			string(w.ChainID), strings.ToLower(w.Pool), w.BlockNumber, strings.ToLower(w.TxHash),
			w.LogIndex, strings.ToLower(w.Trader), a0, a1, w.Buyer, w.USD, w.Time)
	}

	br := s.db.SendBatch(ctx, batch)
	defer br.Close()
	for range swaps {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("store: insert swaps: %w", err)
		}
	}
	return nil
}

// TraderCounts is the honeypot signal's raw material: how many *addresses*
// bought and how many ever sold in the window. Counting swaps instead of
// addresses would make a single bot look like a healthy market.
type TraderCounts struct {
	Buyers    int
	Sellers   int
	VolumeUSD *float64
}

func (s *Store) TraderCounts(ctx context.Context, chainID chain.ID, pool string, window time.Duration) (TraderCounts, error) {
	since := time.Now().Add(-window)
	var tc TraderCounts
	err := s.db.QueryRow(ctx, `
SELECT
  COUNT(DISTINCT trader) FILTER (WHERE buyer),
  COUNT(DISTINCT trader) FILTER (WHERE NOT buyer),
  SUM(ABS(usd))
FROM swaps
WHERE chain_id = $1 AND pool = $2 AND ts >= $3`,
		string(chainID), strings.ToLower(pool), since).Scan(&tc.Buyers, &tc.Sellers, &tc.VolumeUSD)
	if err != nil {
		return TraderCounts{}, fmt.Errorf("store: trader counts: %w", err)
	}
	return tc, nil
}

// PruneSwaps enforces retention. The product only ever asks about the last
// hour; keeping a day is generous headroom for backfills and debugging.
func (s *Store) PruneSwaps(ctx context.Context, keep time.Duration) (int64, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM swaps WHERE ts < $1`, time.Now().Add(-keep))
	if err != nil {
		return 0, fmt.Errorf("store: prune swaps: %w", err)
	}
	return tag.RowsAffected(), nil
}

// UpsertReserves stores the latest pool state used for pricing.
func (s *Store) UpsertReserves(ctx context.Context, chainID chain.ID, pool string, r0, r1, sqrtPrice, liquidity *big.Int, block uint64) error {
	str := func(b *big.Int) *string {
		if b == nil {
			return nil
		}
		v := b.String()
		return &v
	}
	_, err := s.db.Exec(ctx, `
INSERT INTO pool_reserves (chain_id, pool, reserve0, reserve1, sqrt_price, liquidity, block_number, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7, now())
ON CONFLICT (chain_id, pool) DO UPDATE SET
  reserve0 = COALESCE(EXCLUDED.reserve0, pool_reserves.reserve0),
  reserve1 = COALESCE(EXCLUDED.reserve1, pool_reserves.reserve1),
  sqrt_price = COALESCE(EXCLUDED.sqrt_price, pool_reserves.sqrt_price),
  liquidity = COALESCE(EXCLUDED.liquidity, pool_reserves.liquidity),
  block_number = GREATEST(EXCLUDED.block_number, pool_reserves.block_number),
  updated_at = now()`,
		string(chainID), strings.ToLower(pool), str(r0), str(r1), str(sqrtPrice), str(liquidity), block)
	if err != nil {
		return fmt.Errorf("store: upsert reserves: %w", err)
	}
	return nil
}

// Reserves reads back the cached pool state for quoting.
func (s *Store) Reserves(ctx context.Context, chainID chain.ID, pool string) (r0, r1 *big.Int, err error) {
	var a, b *string
	err = s.db.QueryRow(ctx,
		`SELECT reserve0, reserve1 FROM pool_reserves WHERE chain_id = $1 AND pool = $2`,
		string(chainID), strings.ToLower(pool)).Scan(&a, &b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("store: reserves: %w", err)
	}
	parse := func(p *string) *big.Int {
		if p == nil {
			return nil
		}
		n, ok := new(big.Int).SetString(*p, 10)
		if !ok {
			return nil
		}
		return n
	}
	return parse(a), parse(b), nil
}

// GetSwapCursor reports how far the per-pool trade sweep has read.
func (s *Store) GetSwapCursor(ctx context.Context, chainID chain.ID, pool string) (uint64, bool, error) {
	var cursor *int64
	err := s.db.QueryRow(ctx,
		`SELECT swap_cursor FROM pools WHERE chain_id = $1 AND address = $2`,
		string(chainID), strings.ToLower(pool)).Scan(&cursor)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: get swap cursor: %w", err)
	}
	if cursor == nil {
		return 0, false, nil
	}
	return uint64(*cursor), true, nil
}

// SetSwapCursor advances it. GREATEST keeps concurrent enrichment passes from
// walking the cursor backwards and re-reading logs already accounted for.
func (s *Store) SetSwapCursor(ctx context.Context, chainID chain.ID, pool string, block uint64) error {
	_, err := s.db.Exec(ctx,
		`UPDATE pools SET swap_cursor = GREATEST(COALESCE(swap_cursor, 0), $3)
		 WHERE chain_id = $1 AND address = $2`,
		string(chainID), strings.ToLower(pool), int64(block))
	if err != nil {
		return fmt.Errorf("store: set swap cursor: %w", err)
	}
	return nil
}
