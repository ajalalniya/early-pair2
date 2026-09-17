// Package pipeline joins the ingester and the enricher to the stream hub and
// the watchlist matcher. It exists so neither of those has to know that clients
// exist, and so the decisions about what reaches a user live in one place.
package pipeline

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/store"
	"github.com/earlypair/backend/internal/stream"
)

type Sink struct {
	store *store.Store
	hub   *stream.Hub
	log   *slog.Logger
}

func NewSink(st *store.Store, hub *stream.Hub, log *slog.Logger) *Sink {
	return &Sink{store: st, hub: hub, log: log.With("component", "pipeline")}
}

// PoolDetected publishes a newly discovered pool and runs the watch matcher.
func (s *Sink) PoolDetected(ctx context.Context, p chain.Pool) {
	// A backfilled pool is real but not news. Pushing hours-old pairs into a
	// live feed after a restart would bury whatever is actually new.
	if !p.Backfilled {
		s.hub.Publish(stream.TopicPoolsNew, p.ChainID, p)
	}
	s.matchTokenWatches(ctx, p)
}

// PoolStateChanged carries confirmations and reorgs. These go out immediately
// rather than coalesced: a row that turns out to be reorged should stop being
// tradeable on the user's screen as soon as we know.
func (s *Sink) PoolStateChanged(ctx context.Context, p chain.Pool) {
	s.hub.Publish(stream.TopicPoolsUpdate, p.ChainID, map[string]any{
		"chainId": p.ChainID,
		"address": strings.ToLower(p.Address),
		"state":   p.State,
	})
}

// PoolEnriched carries market and risk changes, coalesced per pool so a heavily
// traded pool cannot flood every connected client.
func (s *Sink) PoolEnriched(ctx context.Context, p chain.Pool) {
	s.hub.PublishCoalesced(stream.TopicPoolsUpdate, p.Key(), p.ChainID, p)
}

// PoolMarketUpdated carries a new pair's liquidity, volume, traders and price
// change while the market tracker is watching it. Coalesced per pool, like any
// other update.
func (s *Sink) PoolMarketUpdated(ctx context.Context, p chain.Pool) {
	s.hub.PublishCoalesced(stream.TopicPoolsUpdate, p.Key(), p.ChainID, p)
}

// PoolRemoved tells clients to drop a pair that failed screening. Sent at once,
// not coalesced, and in the same shape as a state change so any client already
// handling pools.update receives it.
func (s *Sink) PoolRemoved(ctx context.Context, p chain.Pool) {
	s.hub.Publish(stream.TopicPoolsUpdate, p.ChainID, map[string]any{
		"chainId":   p.ChainID,
		"address":   strings.ToLower(p.Address),
		"removed":   true,
		"removedAt": p.RemovedAt,
	})
}

// HeadAdvanced drives the UI's block pulse. Coalesced per chain: at one frame
// per block per chain this would otherwise be the noisiest topic on the wire.
func (s *Sink) HeadAdvanced(ctx context.Context, chainID chain.ID, block uint64, at time.Time) {
	s.hub.PublishCoalesced(stream.TopicHead, string(chainID), chainID, map[string]any{
		"chainId":   chainID,
		"block":     block,
		"timestamp": at.UTC(),
	})
}

// TopicMatched publishes an event-topic watch hit.
func (s *Sink) TopicMatched(ctx context.Context, m store.TopicMatch) {
	s.hub.PublishCoalesced(stream.TopicWatchHits, "topic:"+m.Topic0, m.ChainID, map[string]any{
		"kind":        "topic",
		"topic0":      m.Topic0,
		"chainId":     m.ChainID,
		"txHash":      m.TxHash,
		"blockNumber": m.BlockNumber,
		"at":          m.Time,
	})
}

// matchTokenWatches is the token watchlist's whole point: the moment a pair for
// a watched token is deployed anywhere we index, the watcher hears about it.
func (s *Sink) matchTokenWatches(ctx context.Context, p chain.Pool) {
	watches, err := s.store.ActiveTokenWatches(ctx)
	if err != nil {
		s.log.Debug("could not load token watches", "error", err)
		return
	}
	if len(watches) == 0 {
		return
	}

	var t0, t1 string
	if p.Token0 != nil {
		t0 = strings.ToLower(p.Token0.Address)
	}
	if p.Token1 != nil {
		t1 = strings.ToLower(p.Token1.Address)
	}

	for _, watch := range watches {
		addr := strings.ToLower(watch.Address)
		if addr == "" || (addr != t0 && addr != t1) {
			continue
		}
		// A watch scoped to particular chains ignores pools elsewhere.
		if len(watch.Chains) > 0 && !contains(watch.Chains, string(p.ChainID)) {
			continue
		}

		fresh, err := s.store.RecordTokenWatchHit(ctx, watch.ID, p.ChainID, p.Address)
		if err != nil {
			s.log.Debug("could not record a watch hit", "watch", watch.ID, "error", err)
			continue
		}
		if !fresh {
			continue // already alerted; re-processing must not re-notify
		}

		s.hub.Publish(stream.TopicWatchHits, p.ChainID, map[string]any{
			"kind":    "token",
			"watchId": watch.ID,
			"name":    watch.Name,
			"chainId": p.ChainID,
			"pool":    strings.ToLower(p.Address),
			"pair":    p.Pair,
			"dex":     p.Dex,
			"at":      p.CreatedAt,
		})
		s.log.Info("token watch matched",
			"watch", watch.Name, "chain", p.ChainID, "pool", p.Address)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}
