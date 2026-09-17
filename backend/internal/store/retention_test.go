package store

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/config"
)

// These tests need a real Postgres, since retention is a single SQL statement
// whose correctness is the whole point. They run when
// EARLYPAIR_TEST_DATABASE_URL names a disposable database and are skipped
// otherwise. The database is migrated and its tables emptied.
func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("EARLYPAIR_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("EARLYPAIR_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, config.Database{URL: url, AutoMigrate: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	for _, table := range []string{"favorite_pools", "portfolio_trades", "swaps", "pool_reserves", "risk_snapshots", "pools", "tokens"} {
		if _, err := st.db.Exec(ctx, "DELETE FROM "+table); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func addr(n int) string { return fmt.Sprintf("0x%040x", n) }

func insertPool(t *testing.T, st *Store, n int, age time.Duration, token0, token1 string) chain.Pool {
	t.Helper()
	p := chain.Pool{
		ChainID: "arc", Address: addr(n), Dex: "Uniswap v3", Model: chain.ModelCLMM,
		State: chain.StateConfirmed, CreatedAt: time.Now().Add(-age), CreatedBlock: uint64(n),
		CreatedTx: fmt.Sprintf("0x%064x", n), Token0: &chain.Token{Address: token0}, Token1: &chain.Token{Address: token1},
	}
	if _, err := st.InsertPool(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func count(t *testing.T, st *Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPruneOldPoolsKeepsWhatSomeoneStillNeeds(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	usdc, oldTok, keptTok := addr(0x3600), addr(0xA1), addr(0xB1)

	stale := insertPool(t, st, 1, 72*time.Hour, oldTok, usdc)
	starred := insertPool(t, st, 2, 72*time.Hour, keptTok, usdc)
	traded := insertPool(t, st, 3, 72*time.Hour, addr(0xC1), usdc)
	fresh := insertPool(t, st, 4, time.Hour, addr(0xD1), usdc)

	// Rows hanging off the stale pool, which have no foreign key to it.
	mustExec(t, st, `INSERT INTO pool_reserves (chain_id, pool) VALUES ('arc', $1)`, stale.Address)
	mustExec(t, st, `INSERT INTO risk_snapshots (chain_id, pool, score, band, verdict, checks, inputs, weights)
	                 VALUES ('arc', $1, 50, 'watch', '', '[]', '{}', '{}')`, stale.Address)
	mustExec(t, st, `INSERT INTO swaps (chain_id, pool, block_number, tx_hash, log_index, trader, buyer, ts)
	                 VALUES ('arc', $1, 1, '0xabc', 0, '0xdef', true, now())`, stale.Address)

	if err := st.AddFavorite(ctx, starred.ChainID, starred.Address); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertTrade(ctx, Trade{
		ChainID: "arc", TxHash: fmt.Sprintf("0x%064x", 99), Wallet: addr(0xEE), Pool: traded.Address,
		Token: addr(0xC1), Side: "buy", TokenAmount: "1", QuoteToken: usdc, QuoteAmount: "1", BlockNumber: 9,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := st.PruneOldPools(ctx, 48*time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("pruned %d pools, want 1 (only the stale, unstarred, untraded one)", n)
	}
	for _, p := range []chain.Pool{starred, traded, fresh} {
		if count(t, st, `SELECT count(*) FROM pools WHERE address = $1`, p.Address) != 1 {
			t.Errorf("pool %s was deleted but must be kept", p.Address)
		}
	}
	if count(t, st, `SELECT count(*) FROM pools WHERE address = $1`, stale.Address) != 0 {
		t.Error("the stale pool is still there")
	}
	for _, table := range []string{"pool_reserves", "risk_snapshots", "swaps"} {
		if c := count(t, st, `SELECT count(*) FROM `+table+` WHERE pool = $1`, stale.Address); c != 0 {
			t.Errorf("%d %s rows of the deleted pool remain", c, table)
		}
	}
	// A starred pool stays starred: the favorites row must survive too.
	if count(t, st, `SELECT count(*) FROM favorite_pools WHERE address = $1`, starred.Address) != 1 {
		t.Error("the starred pool lost its star")
	}

	// Tokens: one only the deleted pool used, one a kept pool uses, and one
	// orphan touched too recently to be safe to delete.
	mustExec(t, st, `INSERT INTO tokens (chain_id, address, updated_at) VALUES
	                 ('arc', $1, now() - interval '3 days'),
	                 ('arc', $2, now() - interval '3 days'),
	                 ('arc', $3, now())`, oldTok, keptTok, addr(0xF1))
	tn, err := st.PruneOrphanTokens(ctx, 48*time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	if tn != 1 {
		t.Errorf("pruned %d tokens, want 1", tn)
	}
	if count(t, st, `SELECT count(*) FROM tokens WHERE address = $1`, oldTok) != 0 {
		t.Error("the token only the deleted pool used is still there")
	}
	if count(t, st, `SELECT count(*) FROM tokens WHERE address IN ($1, $2)`, keptTok, addr(0xF1)) != 2 {
		t.Error("a token still in use, or too recent to judge, was deleted")
	}
}

func TestPruneOldPoolsWorksThroughABacklogInBatches(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	total := retentionBatch + 250
	mustExec(t, st, `
INSERT INTO pools (chain_id, address, model, state, created_at, created_block, created_tx, log_index)
SELECT 'arc', 'pool-' || g, 'v2', 'confirmed', now() - interval '5 days', g, 'tx-' || g, 0
FROM generate_series(1, $1) g`, total)

	// One batch per call: the backlog is not cleared at once...
	n, err := st.PruneOldPools(ctx, 48*time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(retentionBatch) {
		t.Errorf("first call pruned %d, want one batch of %d", n, retentionBatch)
	}
	// ...and the next call finishes it.
	n, err = st.PruneOldPools(ctx, 48*time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	if n != 250 {
		t.Errorf("second call pruned %d, want the remaining 250", n)
	}
	if c := count(t, st, `SELECT count(*) FROM pools`); c != 0 {
		t.Errorf("%d pools left", c)
	}
}

func TestPruneOldPoolsDisabledAtZero(t *testing.T) {
	st := testStore(t)
	insertPool(t, st, 1, 30*24*time.Hour, addr(0xA1), addr(0x3600))
	if n, err := st.PruneOldPools(context.Background(), 0, 10); err != nil || n != 0 {
		t.Fatalf("retention 0 pruned %d (err %v), want nothing", n, err)
	}
}

func mustExec(t *testing.T, st *Store, q string, args ...any) {
	t.Helper()
	if _, err := st.db.Exec(context.Background(), q, args...); err != nil {
		t.Fatal(err)
	}
}
