// Package store is the persistence layer. Ingestion correctness lives here as
// much as in the poller: the unique index on a pool's creating log is what makes
// replay a no-op, and the cursor table is what makes a restart cost latency
// rather than data.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver, needed by goose
	"github.com/pressly/goose/v3"

	"github.com/earlypair/backend/internal/config"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store wraps the connection pool. It is safe for concurrent use.
type Store struct {
	db  *pgxpool.Pool
	log *slog.Logger
}

// Open connects, verifies reachability, and optionally migrates.
func Open(ctx context.Context, cfg config.Database, log *slog.Logger) (*Store, error) {
	pcfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("store: parse database url: %w", err)
	}
	if cfg.MaxConns > 0 {
		pcfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		pcfg.MinConns = cfg.MinConns
	}
	if cfg.MaxConnIdle > 0 {
		pcfg.MaxConnIdleTime = cfg.MaxConnIdle
	}

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}

	s := &Store{db: pool, log: log.With("component", "store")}

	if cfg.AutoMigrate {
		if err := s.Migrate(ctx, cfg.URL); err != nil {
			pool.Close()
			return nil, err
		}
	}
	return s, nil
}

// Migrate runs the embedded goose migrations. It opens its own database/sql
// handle because goose needs one; the pgx pool stays the runtime path.
func (s *Store) Migrate(ctx context.Context, url string) error {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return fmt.Errorf("store: migrate open: %w", err)
	}
	defer db.Close()

	// The API and the indexer both migrate at startup. Unserialised, both ran
	// a new CREATE TABLE at once and one died on a duplicate type. A blocking
	// advisory lock on a pinned connection makes the second wait, then find
	// nothing left to run.
	lockConn, err := s.db.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("store: migrate lock connection: %w", err)
	}
	defer lockConn.Release()
	migrateKey := advisoryKey("migrations")
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateKey); err != nil {
		return fmt.Errorf("store: migrate lock: %w", err)
	}
	defer func() { _, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrateKey) }()

	goose.SetBaseFS(migrationFS)
	goose.SetLogger(gooseLogger{s.log})
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("store: migrate dialect: %w", err)
	}
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

func (s *Store) Pool() *pgxpool.Pool { return s.db }

func (s *Store) Close() {
	if s.db != nil {
		s.db.Close()
	}
}

// Health is used by the readiness probe.
func (s *Store) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return s.db.Ping(ctx)
}

// gooseLogger adapts slog to goose's logger interface so migration output does
// not bypass the structured log.
type gooseLogger struct{ log *slog.Logger }

func (g gooseLogger) Fatalf(format string, v ...any) {
	g.log.Error("migration failed", "msg", fmt.Sprintf(format, v...))
}

func (g gooseLogger) Printf(format string, v ...any) {
	g.log.Info("migration", "msg", trimNewline(fmt.Sprintf(format, v...)))
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// nullString is a small helper for optional text columns.
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
