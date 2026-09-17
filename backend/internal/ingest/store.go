package ingest

import (
	"context"
	"time"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/store"
)

// Store is the persistence the ingester needs.
//
// It is an interface rather than the concrete *store.Store for one reason: the
// claims this package makes — no pool missed, no pool duplicated, a reorg
// rewound correctly — are claims about its interaction with storage, and they
// are only worth making if they can be tested against a hostile RPC provider
// without standing up a database first.
type Store interface {
	GetCursor(ctx context.Context, chainID chain.ID) (store.Cursor, bool, error)
	SetCursor(ctx context.Context, chainID chain.ID, c store.Cursor) error

	PutBlockHash(ctx context.Context, chainID chain.ID, b store.Block) error
	GetBlockHash(ctx context.Context, chainID chain.ID, number uint64) (string, bool, error)
	DeleteBlockHashesFrom(ctx context.Context, chainID chain.ID, fromBlock uint64) error
	PruneBlockHashes(ctx context.Context, chainID chain.ID, keepFrom uint64) error

	InsertPool(ctx context.Context, p chain.Pool) (bool, error)
	SetPoolState(ctx context.Context, chainID chain.ID, address string, state chain.State) error
	UnconfirmedBelow(ctx context.Context, chainID chain.ID, block uint64, limit int) ([]chain.Pool, error)
	ReorgPoolsFrom(ctx context.Context, chainID chain.ID, fromBlock uint64) ([]string, error)
	RestorePool(ctx context.Context, chainID chain.ID, txHash string, logIndex uint) (bool, error)

	InsertTopicMatches(ctx context.Context, matches []store.TopicMatch) error
	ActiveTopics(ctx context.Context, chainID chain.ID) ([]string, error)

	// Used by the market tracker.
	TrackablePools(ctx context.Context, chainID chain.ID, since time.Time) ([]chain.Pool, error)
	UpdateTrackedMarket(ctx context.Context, chainID chain.ID, address string, m store.Market) error
	ScreenPool(ctx context.Context, chainID chain.ID, address string, keep bool) (time.Time, error)
	GetPool(ctx context.Context, chainID chain.ID, address string) (chain.Pool, error)
	GetToken(ctx context.Context, chainID chain.ID, address string) (store.TokenRecord, error)
	NativePrice(ctx context.Context, chainID chain.ID) (float64, bool, error)
}

// Compile-time proof that the real store satisfies it.
var _ Store = (*store.Store)(nil)
