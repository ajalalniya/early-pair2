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

// AddRPCEndpoint persists an endpoint added at runtime so it survives a
// restart. The URL is stored as given because the pool needs to dial it; it is
// masked everywhere it is displayed or logged.
func (s *Store) AddRPCEndpoint(ctx context.Context, id string, chainID chain.ID,
	transport chain.Transport, url string, rateLimit float64) error {
	_, err := s.db.Exec(ctx, `
INSERT INTO rpc_endpoints (id, chain_id, transport, url, rate_limit)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (id) DO UPDATE SET url = EXCLUDED.url, rate_limit = EXCLUDED.rate_limit`,
		id, string(chainID), string(transport), url, rateLimit)
	if err != nil {
		return fmt.Errorf("store: add rpc endpoint: %w", err)
	}
	return nil
}

// StoredEndpoint is a runtime-added endpoint, read back at startup so an
// operator's addition is not lost on the next deploy.
type StoredEndpoint struct {
	ID        string
	ChainID   chain.ID
	Transport chain.Transport
	URL       string
	RateLimit float64
}

func (s *Store) ListRPCEndpoints(ctx context.Context) ([]StoredEndpoint, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, chain_id, transport, url, rate_limit FROM rpc_endpoints`)
	if err != nil {
		return nil, fmt.Errorf("store: list rpc endpoints: %w", err)
	}
	defer rows.Close()

	var out []StoredEndpoint
	for rows.Next() {
		var e StoredEndpoint
		var chainID, transport string
		if err := rows.Scan(&e.ID, &chainID, &transport, &e.URL, &e.RateLimit); err != nil {
			return nil, err
		}
		e.ChainID = chain.ID(chainID)
		e.Transport = chain.Transport(transport)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PutNonce issues a single-use login nonce.
func (s *Store) PutNonce(ctx context.Context, nonce, address string, expires time.Time) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO auth_nonces (nonce, address, expires_at) VALUES ($1,$2,$3)`,
		nonce, strings.ToLower(address), expires)
	if err != nil {
		return fmt.Errorf("store: put nonce: %w", err)
	}
	return nil
}

// ConsumeNonce marks a nonce used and returns who it was issued to.
//
// The update is conditional and atomic: two concurrent verifications of the
// same nonce cannot both succeed, which is the entire point of a nonce.
func (s *Store) ConsumeNonce(ctx context.Context, nonce string) (address string, expires time.Time, err error) {
	err = s.db.QueryRow(ctx, `
UPDATE auth_nonces SET used = TRUE
WHERE nonce = $1 AND NOT used AND expires_at > now()
RETURNING address, expires_at`, nonce).Scan(&address, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, ErrNotFound
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("store: consume nonce: %w", err)
	}
	return address, expires, nil
}

// PruneNonces clears expired login nonces.
func (s *Store) PruneNonces(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `DELETE FROM auth_nonces WHERE expires_at < now() - interval '1 hour'`)
	if err != nil {
		return fmt.Errorf("store: prune nonces: %w", err)
	}
	return nil
}

// SetNativePrice records a chain's native-token price, derived by the enricher
// from that chain's reference pool. The API reads it to price gas; nothing here
// comes from an external feed.
func (s *Store) SetNativePrice(ctx context.Context, chainID chain.ID, usd float64) error {
	_, err := s.db.Exec(ctx, `
INSERT INTO chain_prices (chain_id, native_usd, updated_at)
VALUES ($1,$2, now())
ON CONFLICT (chain_id) DO UPDATE SET native_usd = EXCLUDED.native_usd, updated_at = now()`,
		string(chainID), usd)
	if err != nil {
		return fmt.Errorf("store: set native price: %w", err)
	}
	return nil
}

// NativePrice reads it back.
//
// A price older than ten minutes is reported as unavailable rather than used: a
// stale price on a volatile asset is a wrong number wearing the costume of a
// right one, and everything downstream would present it as current.
func (s *Store) NativePrice(ctx context.Context, chainID chain.ID) (float64, bool, error) {
	var (
		usd     float64
		updated time.Time
	)
	err := s.db.QueryRow(ctx,
		`SELECT native_usd, updated_at FROM chain_prices WHERE chain_id = $1`,
		string(chainID)).Scan(&usd, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: native price: %w", err)
	}
	if time.Since(updated) > 10*time.Minute {
		return 0, false, nil
	}
	return usd, true, nil
}
