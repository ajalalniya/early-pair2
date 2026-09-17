// Command earlypair-api serves the REST and WebSocket surface.
//
// It is deliberately separate from the indexer: the API tier scales
// horizontally behind a load balancer, while exactly one indexer owns each
// chain. Running two indexers on one chain would double the RPC bill to
// discover the same pools twice.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/earlypair/backend/internal/api"
	"github.com/earlypair/backend/internal/config"
	"github.com/earlypair/backend/internal/obs"
	"github.com/earlypair/backend/internal/risk"
	"github.com/earlypair/backend/internal/rpcpool"
	"github.com/earlypair/backend/internal/store"
	"github.com/earlypair/backend/internal/stream"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config/earlypair.yaml", "path to the configuration file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	log := obs.SetupLogger(cfg.Log.Level, cfg.Log.Format)

	if cfg.Auth.JWTSecret == "" {
		// Without a secret the authenticated endpoints cannot issue or verify a
		// session, and failing at startup is far better than failing per request.
		return fmt.Errorf("no auth.jwtSecret configured (set EARLYPAIR_JWT_SECRET)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.Database, log)
	if err != nil {
		return err
	}
	defer st.Close()

	reg := cfg.Registry()

	// Endpoints added at runtime live in the database, so they survive a deploy.
	endpoints := cfg.Endpoints
	if stored, err := st.ListRPCEndpoints(ctx); err == nil {
		for _, e := range stored {
			endpoints = append(endpoints, config.Endpoint{
				ID: e.ID, ChainID: e.ChainID, Transport: e.Transport,
				URL: e.URL, RateLimit: e.RateLimit,
			})
		}
	} else {
		log.Warn("could not load stored rpc endpoints", "error", err)
	}

	pool, err := rpcpool.New(cfg.RPC, endpoints, reg, log)
	if err != nil {
		return err
	}

	hub := stream.NewHub(cfg.Stream, log)
	scorer := risk.New(cfg.Risk)
	server := api.NewServer(cfg, reg, st, pool, hub, scorer, log)

	// Every live event originates in the indexer, a separate process. The bus
	// is the only way it reaches this hub, and so the only way a new pool
	// reaches a connected browser.
	var bus *stream.Bus
	if cfg.Redis.Disabled {
		log.Warn("redis disabled: REST works, but WebSocket clients will receive no live events")
	} else {
		bus, err = stream.NewBus(ctx, cfg.Redis.URL, log)
		if err != nil {
			return fmt.Errorf("%w (set EARLYPAIR_REDIS_DISABLED=true to run without a live feed)", err)
		}
		defer bus.Close()
		server.SetBus(bus)
	}

	httpSrv := &http.Server{
		Addr:    cfg.Server.Addr,
		Handler: server.Routes(),
		// No WriteTimeout: it would guillotine long-lived WebSocket connections
		// mid-stream. Per-request deadlines come from the router's middleware.
		ReadHeaderTimeout: cfg.Server.ReadTimeout,
	}

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsSrv := &http.Server{
		Addr:              cfg.Server.MetricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return pool.Run(gctx) })
	g.Go(func() error { return hub.Run(gctx) })
	g.Go(func() error { return janitor(gctx, st, cfg.Ingest.PoolRetention, log) })
	if bus != nil {
		g.Go(func() error { return bus.RunSubscriber(gctx, hub) })
	} else {
		// Without the indexer's snapshot, this process's own view is the only
		// health there is to push; the REST response says which it is.
		g.Go(func() error { return publishHealth(gctx, pool, hub) })
	}

	g.Go(func() error {
		log.Info("api listening", "addr", cfg.Server.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		log.Info("metrics listening", "addr", cfg.Server.MetricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})

	g.Go(func() error {
		<-gctx.Done()
		// Close client sockets with a going-away status so they reconnect
		// cleanly to another instance rather than treating it as an error.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
		defer cancel()
		log.Info("shutting down")
		_ = metricsSrv.Shutdown(shutdownCtx)
		return httpSrv.Shutdown(shutdownCtx)
	})

	err = g.Wait()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// janitor keeps the unbounded tables bounded. Every one of these deletes data
// the product has already stopped asking about.
func janitor(ctx context.Context, st *store.Store, poolRetention time.Duration, log *slog.Logger) error {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if n, err := st.PruneSwaps(ctx, 48*time.Hour); err != nil {
				log.Debug("swap prune failed", "error", err)
			} else if n > 0 {
				log.Debug("pruned swaps", "rows", n)
			}
			if err := st.PruneTopicMatches(ctx); err != nil {
				log.Debug("topic match prune failed", "error", err)
			}
			if err := st.PruneNonces(ctx); err != nil {
				log.Debug("nonce prune failed", "error", err)
			}
			// A pool that has stayed reorged for a day is not coming back; its
			// row only exists to keep the rescan from re-announcing it.
			if n, err := st.PurgeReorged(ctx, 24*time.Hour); err != nil {
				log.Debug("reorged pool purge failed", "error", err)
			} else if n > 0 {
				log.Debug("purged reorged pools", "rows", n)
			}
			// Old pools go, bounded per tick so a first run on a large backlog
			// clears it over several ticks instead of one long delete. Tokens
			// only a deleted pool used go after them.
			if n, err := st.PruneOldPools(ctx, poolRetention, 40); err != nil {
				log.Warn("old pool prune failed", "error", err)
			} else if n > 0 {
				log.Info("pruned old pools", "rows", n, "olderThan", poolRetention)
			}
			if n, err := st.PruneOrphanTokens(ctx, poolRetention, 40); err != nil {
				log.Warn("orphan token prune failed", "error", err)
			} else if n > 0 {
				log.Info("pruned orphan tokens", "rows", n)
			}
			// A topic matching faster than the ceiling is suspended rather than
			// allowed to consume the chain's whole log budget.
			if ids, err := st.SuspendNoisyTopics(ctx, 50_000); err != nil {
				log.Debug("noisy topic check failed", "error", err)
			} else if len(ids) > 0 {
				log.Warn("suspended topic watches for exceeding the match-rate ceiling",
					"count", len(ids), "ids", ids)
			}
		}
	}
}

// publishHealth pushes the RPC health snapshot to subscribed clients so the
// health panel is live without polling.
func publishHealth(ctx context.Context, pool *rpcpool.Pool, hub *stream.Hub) error {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			hub.PublishCoalesced(stream.TopicRPCHealth, "all", "", pool.Status())
		}
	}
}
