// Package stream is the live fan-out to connected clients.
//
// Two properties matter here. A monotonic sequence number plus a short replay
// buffer make a client's reconnect lossless: it says "I last saw 918230" and
// gets exactly what it missed, which is what lets the frontend treat a dropped
// socket as a hiccup rather than a reason to refetch everything. And a bounded
// per-client send queue means one slow consumer is disconnected instead of
// growing the server's memory until everyone suffers.
package stream

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/config"
	"github.com/earlypair/backend/internal/obs"
)

// Topics the server publishes.
const (
	TopicPoolsNew    = "pools.new"
	TopicPoolsUpdate = "pools.update"
	TopicHead        = "head"
	TopicRPCHealth   = "rpc.health"
	TopicWatchHits   = "watch.hits"
	TopicPortfolio   = "portfolio"
	TopicResync      = "resync"
)

// Frame is the wire format. Field names are deliberately terse: at several
// frames per second per client, the envelope should not dominate the payload.
type Frame struct {
	T     string   `json:"t"`
	Seq   uint64   `json:"seq"`
	TS    int64    `json:"ts"`
	Data  any      `json:"d,omitempty"`
	Chain chain.ID `json:"chain,omitempty"`
}

// Hub owns the client set, the sequence counter and the replay buffer.
type Hub struct {
	cfg config.Stream
	log *slog.Logger

	seq atomic.Uint64

	mu      sync.RWMutex
	clients map[*Client]struct{}

	replayMu sync.Mutex
	replay   []Frame

	// coalesce holds the most recent pending update per key, flushed on a
	// timer. A pool being traded heavily produces a burst of updates that no
	// human can perceive individually; sending all of them just costs bandwidth
	// and forces the client to re-render.
	coalesceMu sync.Mutex
	coalesced  map[string]Frame

	// forward, when set, copies every published event onto the cross-process
	// bus. Only a producing process (the indexer) sets it; a consuming process
	// (the API) feeds its hub from the bus and must not forward back, or every
	// event would loop.
	forward Forwarder
}

// Forwarder receives each published event with its payload already encoded.
type Forwarder func(topic string, chainID chain.ID, key string, data json.RawMessage)

// SetForwarder installs the bus hook. Call it before the hub starts publishing.
func (h *Hub) SetForwarder(f Forwarder) { h.forward = f }

func (h *Hub) forwardEvent(topic string, chainID chain.ID, key string, data any) {
	if h.forward == nil {
		return
	}
	raw, ok := data.(json.RawMessage)
	if !ok {
		b, err := json.Marshal(data)
		if err != nil {
			h.log.Warn("event not forwarded: payload does not encode", "topic", topic, "error", err)
			return
		}
		raw = b
	}
	h.forward(topic, chainID, key, raw)
}

func NewHub(cfg config.Stream, log *slog.Logger) *Hub {
	return &Hub{
		cfg:       cfg,
		log:       log.With("component", "stream"),
		clients:   map[*Client]struct{}{},
		coalesced: map[string]Frame{},
	}
}

// Run flushes coalesced updates and prunes the replay buffer.
func (h *Hub) Run(ctx context.Context) error {
	window := h.cfg.CoalesceWindow
	if window <= 0 {
		window = 250 * time.Millisecond
	}
	flush := time.NewTicker(window)
	defer flush.Stop()

	prune := time.NewTicker(30 * time.Second)
	defer prune.Stop()

	for {
		select {
		case <-ctx.Done():
			h.closeAll()
			return ctx.Err()
		case <-flush.C:
			h.flushCoalesced()
		case <-prune.C:
			h.pruneReplay()
		}
	}
}

// Publish sends a frame immediately. Used for events a user is waiting on — a
// new pool above all — where latency is the entire point.
func (h *Hub) Publish(topic string, chainID chain.ID, data any) {
	f := Frame{
		T:     topic,
		Seq:   h.seq.Add(1),
		TS:    time.Now().UnixMilli(),
		Data:  data,
		Chain: chainID,
	}
	h.record(f)
	h.broadcast(f)
	obs.StreamFrames.WithLabelValues(topic).Inc()
	h.forwardEvent(topic, chainID, "", data)
}

// PublishCoalesced replaces any pending frame with the same key. The key should
// identify the entity, not the event, so the newest state wins.
func (h *Hub) PublishCoalesced(topic, key string, chainID chain.ID, data any) {
	h.coalesceMu.Lock()
	h.coalesced[topic+"|"+key] = Frame{T: topic, Data: data, Chain: chainID}
	h.coalesceMu.Unlock()
	// Forwarded uncoalesced: the receiving hub coalesces on its own window,
	// and coalescing twice would only add latency.
	h.forwardEvent(topic, chainID, key, data)
}

func (h *Hub) flushCoalesced() {
	h.coalesceMu.Lock()
	if len(h.coalesced) == 0 {
		h.coalesceMu.Unlock()
		return
	}
	pending := h.coalesced
	h.coalesced = map[string]Frame{}
	h.coalesceMu.Unlock()

	for _, f := range pending {
		f.Seq = h.seq.Add(1)
		f.TS = time.Now().UnixMilli()
		h.record(f)
		h.broadcast(f)
		obs.StreamFrames.WithLabelValues(f.T).Inc()
	}
}

func (h *Hub) broadcast(f Frame) {
	h.mu.RLock()
	clients := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	for _, c := range clients {
		if !c.wants(f) {
			continue
		}
		if !c.trySend(f) {
			// The client cannot keep up. Dropping it is the correct outcome:
			// the alternative is unbounded memory growth on the server to
			// serve one stalled browser tab. It reconnects and resyncs.
			h.log.Info("dropping slow client", "client", c.id, "queue", cap(c.out))
			obs.StreamDropped.Inc()
			c.closeWithResync()
			h.Remove(c)
		}
	}
}

// record appends to the replay buffer under both bounds.
func (h *Hub) record(f Frame) {
	maxFrames := h.cfg.ReplayMaxFrames
	if maxFrames <= 0 {
		maxFrames = 20_000
	}
	h.replayMu.Lock()
	h.replay = append(h.replay, f)
	if len(h.replay) > maxFrames {
		h.replay = h.replay[len(h.replay)-maxFrames:]
	}
	h.replayMu.Unlock()
}

func (h *Hub) pruneReplay() {
	window := h.cfg.ReplayWindow
	if window <= 0 {
		window = 5 * time.Minute
	}
	cutoff := time.Now().Add(-window).UnixMilli()

	h.replayMu.Lock()
	defer h.replayMu.Unlock()
	idx := 0
	for idx < len(h.replay) && h.replay[idx].TS < cutoff {
		idx++
	}
	if idx > 0 {
		h.replay = append([]Frame(nil), h.replay[idx:]...)
	}
}

// Since returns the frames a reconnecting client missed, filtered to what it
// subscribes to. The second return reports whether the replay buffer still
// reaches back far enough; when it does not, the client is told to resync
// rather than silently handed an incomplete history.
func (h *Hub) Since(seq uint64, sub Subscription) ([]Frame, bool) {
	h.replayMu.Lock()
	defer h.replayMu.Unlock()

	if len(h.replay) == 0 {
		return nil, h.seq.Load() == seq
	}
	oldest := h.replay[0].Seq
	if seq+1 < oldest {
		return nil, false // gap: the client was away longer than the buffer
	}

	var out []Frame
	for _, f := range h.replay {
		if f.Seq <= seq {
			continue
		}
		if sub.wants(f) {
			out = append(out, f)
		}
	}
	return out, true
}

// Seq exposes the current sequence so a fresh client can start from "now".
func (h *Hub) Seq() uint64 { return h.seq.Load() }

func (h *Hub) Add(c *Client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	n := len(h.clients)
	h.mu.Unlock()
	obs.StreamClients.Set(float64(n))
}

func (h *Hub) Remove(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	n := len(h.clients)
	h.mu.Unlock()
	obs.StreamClients.Set(float64(n))
}

func (h *Hub) closeAll() {
	h.mu.Lock()
	clients := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.clients = map[*Client]struct{}{}
	h.mu.Unlock()

	for _, c := range clients {
		c.close()
	}
	obs.StreamClients.Set(0)
}

func (h *Hub) Clients() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}
