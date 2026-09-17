package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/obs"
)

// The bus carries events from the indexer process to the API process.
//
// They are separate binaries on purpose — the API scales out, the indexer owns
// each chain alone — which means the indexer's hub has no browsers attached and
// the API's hub has nothing publishing into it. Without this bridge a client
// connected to the API receives no new pools, no head updates and no watch
// hits at all, however well ingestion is working.
//
// Redis pub/sub is at-most-once: anything published while a subscriber is
// disconnected is gone. So every message carries its producer's identity and a
// per-producer sequence number, and a subscriber that sees a gap tells its
// clients to resync rather than letting them believe their feed is complete.

const (
	busChannel     = "earlypair:stream"
	controlChannel = "earlypair:control"
	healthKey      = "earlypair:rpc-health"
	healthFreshFor = 30 * time.Second
)

// Control is an operator action aimed at the process that owns ingestion.
// The health panel's buttons are served by the API, but the endpoints they act
// on — the ones actually carrying pool detection — live in the indexer. Acting
// on the API's own pool would change nothing that matters.
type Control struct {
	Action     string `json:"action"`
	EndpointID string `json:"endpointId"`
}

// SendControl publishes an action for the indexer. It reports whether any
// process was subscribed to receive it, so the API can tell the operator the
// truth when no indexer is running.
func (b *Bus) SendControl(ctx context.Context, c Control) (bool, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return false, err
	}
	n, err := b.client.Publish(ctx, controlChannel, payload).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// RunControl applies control actions as they arrive.
func (b *Bus) RunControl(ctx context.Context, apply func(Control)) error {
	for {
		sub := b.client.Subscribe(ctx, controlChannel)
		ch := sub.Channel()
		for msg := range ch {
			var c Control
			if err := json.Unmarshal([]byte(msg.Payload), &c); err == nil {
				apply(c)
			}
		}
		sub.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Message is one event on the bus.
type Message struct {
	Producer string          `json:"p"`
	Seq      uint64          `json:"s"`
	Topic    string          `json:"t"`
	Chain    chain.ID        `json:"c,omitempty"`
	Key      string          `json:"k,omitempty"` // set for coalesced topics
	Data     json.RawMessage `json:"d"`
}

// Bus is a Redis-backed event bridge.
type Bus struct {
	client   *redis.Client
	log      *slog.Logger
	producer string
	seq      atomic.Uint64
	out      chan Message

	mu      sync.Mutex
	lastSeq map[string]uint64
}

// NewBus connects to Redis and verifies it is reachable, so a misconfigured
// URL fails at startup instead of silently producing an empty live feed.
func NewBus(ctx context.Context, url string, log *slog.Logger) (*Bus, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("stream: parse redis url: %w", err)
	}
	client := redis.NewClient(opts)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("stream: redis unreachable: %w", err)
	}

	return &Bus{
		client:   client,
		log:      log.With("component", "bus"),
		producer: fmt.Sprintf("%x", time.Now().UnixNano()),
		out:      make(chan Message, 4096),
		lastSeq:  map[string]uint64{},
	}, nil
}

func (b *Bus) Close() error { return b.client.Close() }

// Forward queues a message for publication. It never blocks: it is called from
// the ingestion hot path, and a stalled Redis must not slow pool detection. A
// dropped message becomes a sequence gap, which subscribers turn into a resync.
func (b *Bus) Forward(topic string, chainID chain.ID, key string, data json.RawMessage) {
	m := Message{
		Producer: b.producer,
		Seq:      b.seq.Add(1),
		Topic:    topic,
		Chain:    chainID,
		Key:      key,
		Data:     data,
	}
	select {
	case b.out <- m:
	default:
		obs.BusDropped.Inc()
	}
}

// RunPublisher drains the queue into Redis.
func (b *Bus) RunPublisher(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m := <-b.out:
			payload, err := json.Marshal(m)
			if err != nil {
				continue
			}
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err = b.client.Publish(pctx, busChannel, payload).Err()
			cancel()
			if err != nil && ctx.Err() == nil {
				obs.BusDropped.Inc()
				b.log.Warn("could not publish to redis; subscribers will resync", "error", err)
			}
		}
	}
}

// RunSubscriber feeds bus messages into a local hub.
//
// It is the API's only source of live events. The subscription reconnects on
// its own, and any messages lost across a reconnect show up as a sequence gap.
func (b *Bus) RunSubscriber(ctx context.Context, hub *Hub) error {
	for {
		if err := b.subscribeOnce(ctx, hub); err != nil && ctx.Err() == nil {
			b.log.Warn("redis subscription ended; reconnecting", "error", err)
			hub.Publish(TopicResync, "", map[string]string{"reason": "live event bus reconnected"})
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		return ctx.Err()
	}
}

func (b *Bus) subscribeOnce(ctx context.Context, hub *Hub) error {
	sub := b.client.Subscribe(ctx, busChannel)
	defer sub.Close()

	if _, err := sub.Receive(ctx); err != nil {
		return err
	}
	b.log.Info("subscribed to live event bus", "channel", busChannel)

	ch := sub.Channel(redis.WithChannelHealthCheckInterval(15 * time.Second))
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return errors.New("subscription channel closed")
			}
			var m Message
			if err := json.Unmarshal([]byte(msg.Payload), &m); err != nil {
				continue
			}
			if b.gap(m) {
				obs.BusGaps.Inc()
				hub.Publish(TopicResync, "", map[string]string{"reason": "live events were missed"})
			}
			if m.Key != "" {
				hub.PublishCoalesced(m.Topic, m.Key, m.Chain, m.Data)
			} else {
				hub.Publish(m.Topic, m.Chain, m.Data)
			}
		}
	}
}

// gap reports whether messages from this producer were lost. A producer seen
// for the first time is not a gap: it has just started, or we have.
func (b *Bus) gap(m Message) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	last, seen := b.lastSeq[m.Producer]
	b.lastSeq[m.Producer] = m.Seq
	return seen && m.Seq != last+1
}

// PutHealth stores the indexer's RPC health snapshot for the API to serve.
func (b *Bus) PutHealth(ctx context.Context, snapshot any) error {
	payload, err := json.Marshal(struct {
		At        time.Time `json:"at"`
		Endpoints any       `json:"endpoints"`
	}{time.Now().UTC(), snapshot})
	if err != nil {
		return err
	}
	return b.client.Set(ctx, healthKey, payload, 2*healthFreshFor).Err()
}

// Health returns the indexer's latest snapshot, if one is recent enough to be
// worth showing. A stale snapshot is reported as absent: an old "HEALTHY" is
// worse than an honest "unknown".
func (b *Bus) Health(ctx context.Context) (json.RawMessage, time.Time, bool) {
	raw, err := b.client.Get(ctx, healthKey).Bytes()
	if err != nil {
		return nil, time.Time{}, false
	}
	var snap struct {
		At        time.Time       `json:"at"`
		Endpoints json.RawMessage `json:"endpoints"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, time.Time{}, false
	}
	if time.Since(snap.At) > healthFreshFor {
		return nil, snap.At, false
	}
	return snap.Endpoints, snap.At, true
}
