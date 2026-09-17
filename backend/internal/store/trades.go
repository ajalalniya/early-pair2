package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/earlypair/backend/internal/chain"
)

// Trade is one verified swap made through the service.
type Trade struct {
	ChainID     chain.ID
	TxHash      string
	Wallet      string
	Pool        string
	Token       string
	Side        string // "buy" | "sell"
	TokenAmount string // base units
	QuoteToken  string
	QuoteAmount string // base units
	QuoteUSD    *float64
	BlockNumber uint64
	RecordedAt  time.Time
}

// InsertTrade records a verified trade. Recording the same transaction again is
// a no-op; it reports whether this call wrote the row.
func (s *Store) InsertTrade(ctx context.Context, t Trade) (bool, error) {
	tag, err := s.db.Exec(ctx, `
INSERT INTO portfolio_trades (chain_id, tx_hash, wallet, pool, token, side, token_amount,
                              quote_token, quote_amount, quote_usd, block_number)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
ON CONFLICT DO NOTHING`,
		string(t.ChainID), strings.ToLower(t.TxHash), strings.ToLower(t.Wallet), strings.ToLower(t.Pool),
		strings.ToLower(t.Token), t.Side, t.TokenAmount, strings.ToLower(t.QuoteToken), t.QuoteAmount,
		t.QuoteUSD, t.BlockNumber)
	if err != nil {
		return false, fmt.Errorf("store: insert trade: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// HideHolding removes a token from a wallet's holdings list until a newer trade
// in it is recorded. Hiding it again moves the mark forward.
func (s *Store) HideHolding(ctx context.Context, wallet string, chainID chain.ID, token string) error {
	_, err := s.db.Exec(ctx, `
INSERT INTO portfolio_hidden (wallet, chain_id, token) VALUES ($1,$2,$3)
ON CONFLICT (wallet, chain_id, token) DO UPDATE SET hidden_at = now()`,
		strings.ToLower(wallet), string(chainID), strings.ToLower(token))
	if err != nil {
		return fmt.Errorf("store: hide holding: %w", err)
	}
	return nil
}

// HiddenHoldings returns when each of a wallet's hidden tokens was hidden,
// keyed "chain:token".
func (s *Store) HiddenHoldings(ctx context.Context, wallet string) (map[string]time.Time, error) {
	rows, err := s.db.Query(ctx, `
SELECT chain_id, token, hidden_at FROM portfolio_hidden WHERE wallet = $1`, strings.ToLower(wallet))
	if err != nil {
		return nil, fmt.Errorf("store: hidden holdings: %w", err)
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var chainID, token string
		var at time.Time
		if err := rows.Scan(&chainID, &token, &at); err != nil {
			return nil, fmt.Errorf("store: scan hidden holding: %w", err)
		}
		out[chainID+":"+token] = at
	}
	return out, rows.Err()
}

// TradesForWallet returns a wallet's recorded trades, oldest first.
func (s *Store) TradesForWallet(ctx context.Context, wallet string) ([]Trade, error) {
	rows, err := s.db.Query(ctx, `
SELECT chain_id, tx_hash, wallet, pool, token, side, token_amount, quote_token, quote_amount,
       quote_usd, block_number, recorded_at
FROM portfolio_trades
WHERE wallet = $1
ORDER BY chain_id, block_number, recorded_at`, strings.ToLower(wallet))
	if err != nil {
		return nil, fmt.Errorf("store: trades for wallet: %w", err)
	}
	defer rows.Close()
	var out []Trade
	for rows.Next() {
		var t Trade
		var chainID string
		if err := rows.Scan(&chainID, &t.TxHash, &t.Wallet, &t.Pool, &t.Token, &t.Side, &t.TokenAmount,
			&t.QuoteToken, &t.QuoteAmount, &t.QuoteUSD, &t.BlockNumber, &t.RecordedAt); err != nil {
			return nil, fmt.Errorf("store: scan trade: %w", err)
		}
		t.ChainID = chain.ID(chainID)
		out = append(out, t)
	}
	return out, rows.Err()
}
