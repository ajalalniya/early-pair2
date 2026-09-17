// Command earlypair-indexer owns ingestion and enrichment.
//
// Exactly one instance should own a given chain. It takes a Postgres advisory
// lock per chain to enforce that: two indexers on one chain would double the
// RPC bill to discover the same pools twice, which on free public endpoints is
// the difference between working and being rate limited into uselessness.
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

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/config"
	"github.com/earlypair/backend/internal/enrich"
	"github.com/earlypair/backend/internal/ingest"
	"github.com/earlypair/backend/internal/obs"
	"github.com/earlypair/backend/internal/pipeline"
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
	only := flag.String("chains", "", "comma-separated chain ids to run (default: all enabled)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	log := obs.SetupLogger(cfg.Log.Level, cfg.Log.Format)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.Database, log)
	if err != nil {
		return err
	}
	defer st.Close()

	reg := cfg.Registry()

	endpoints := cfg.Endpoints
	if stored, err := st.ListRPCEndpoints(ctx); err == nil {
		for _, e := range stored {
			endpoints = append(endpoints, config.Endpoint{
				ID: e.ID, ChainID: e.ChainID, Transport: e.Transport,
				URL: e.URL, RateLimit: e.RateLimit,
			})
		}
	}

	pool, err := rpcpool.New(cfg.RPC, endpoints, reg, log)
	if err != nil {
		return err
	}

	// The indexer's hub has no clients of its own: browsers connect to the API,
	// a separate process. Every event it publishes is forwarded over Redis to
	// the API's hub, which is the only way a new pool reaches a user live.
	hub := stream.NewHub(cfg.Stream, log)

	var bus *stream.Bus
	if cfg.Redis.Disabled {
		log.Warn("redis disabled: pools are indexed and served over REST, " +
			"but no live events reach API clients and the health panel shows only the API's view")
	} else {
		bus, err = stream.NewBus(ctx, cfg.Redis.URL, log)
		if err != nil {
			// Fatal rather than degraded: running on without the bus is exactly
			// how a live feed ends up silently empty. Opting out is explicit.
			return fmt.Errorf("%w (set EARLYPAIR_REDIS_DISABLED=true to run without a live feed)", err)
		}
		defer bus.Close()
		hub.SetForwarder(bus.Forward)
	}
	sink := pipeline.NewSink(st, hub, log)
	scorer := risk.New(cfg.Risk)
	enricher := enrich.New(cfg.Ingest, reg, pool, st, scorer, cfg.Explorers, sink, log)

	wanted := parseChainFilter(*only)

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
	g.Go(func() error { return enricher.Run(gctx) })
	if bus != nil {
		g.Go(func() error { return bus.RunPublisher(gctx) })
		g.Go(func() error { return publishHealth(gctx, pool, hub, bus, log) })
		g.Go(func() error { return bus.RunControl(gctx, controlHandler(gctx, pool, log)) })
	}
	g.Go(func() error {
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})

	started := 0
	for _, ch := range reg.All() {
		if len(wanted) > 0 && !wanted[ch.ID] {
			continue
		}
		ch := ch

		// One indexer per chain, enforced by the database rather than by
		// convention. A second instance simply skips the chain and says so.
		locked, release, err := st.TryChainLock(gctx, ch.ID)
		if err != nil {
			log.Warn("could not take the chain lock", "chain", ch.ID, "error", err)
			continue
		}
		if !locked {
			log.Info("another indexer owns this chain; skipping", "chain", ch.ID)
			continue
		}

		started++
		ing := ingest.New(ch, cfg.Ingest, pool, st, sink, log)
		var peers []chain.ID
		for _, other := range reg.All() {
			if other.ID != ch.ID && other.NativeSymbol != "" && other.NativeSymbol == ch.NativeSymbol {
				peers = append(peers, other.ID)
			}
		}
		ing.SetNativePricePeers(peers)
		g.Go(func() error {
			defer release()
			err := ing.Run(gctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				// One chain failing must not take the process down with it: the
				// others are still producing.
				log.Error("chain ingester stopped", "chain", ch.ID, "error", err)
				<-gctx.Done()
			}
			return nil
		})
	}

	if started == 0 {
		return fmt.Errorf("no chains to index: every configured chain is disabled, filtered out, or owned by another instance")
	}
	log.Info("indexer running", "chains", started)

	g.Go(func() error {
		<-gctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
		defer cancel()
		return metricsSrv.Shutdown(shutdownCtx)
	})

	err = g.Wait()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// publishHealth shares this process's endpoint health, which is the one that
// matters: these are the endpoints carrying pool detection. The API serves the
// stored snapshot on /rpc/health and relays the frame to live clients.
func publishHealth(ctx context.Context, pool *rpcpool.Pool, hub *stream.Hub, bus *stream.Bus, log *slog.Logger) error {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			status := pool.Status()
			if err := bus.PutHealth(ctx, status); err != nil && ctx.Err() == nil {
				log.Debug("could not store health snapshot", "error", err)
			}
			hub.PublishCoalesced(stream.TopicRPCHealth, "all", "", status)
		}
	}
}

// controlHandler applies the health panel's actions to this process's pool.
func controlHandler(ctx context.Context, pool *rpcpool.Pool, log *slog.Logger) func(stream.Control) {
	return func(c stream.Control) {
		ep, ok := pool.ByID(c.EndpointID)
		if !ok {
			log.Warn("control action for an endpoint this indexer does not have",
				"action", c.Action, "endpoint", c.EndpointID)
			return
		}
		switch c.Action {
		case "resubscribe", "failover":
			ep.ClearPenalties()
			log.Info("endpoint penalties cleared by operator", "action", c.Action, "endpoint", ep.ID)
		case "ping":
			pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			head, err := pool.PingEndpoint(pctx, ep)
			cancel()
			log.Info("endpoint pinged by operator", "endpoint", ep.ID, "head", head, "error", err)
		}
	}
}

func parseChainFilter(raw string) map[chain.ID]bool {
	if raw == "" {
		return nil
	}
	out := map[chain.ID]bool{}
	for _, part := range splitComma(raw) {
		out[chain.ID(part)] = true
	}
	return out
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		if r != ' ' {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
