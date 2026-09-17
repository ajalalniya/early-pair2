package stream

import (
	"sync"

	"github.com/earlypair/backend/internal/chain"
)

// Subscription is what a client asked for.
type Subscription struct {
	Topics map[string]bool
	Chains map[chain.ID]bool
}

// wants decides whether a frame belongs to this subscription. An empty chain
// filter means "all chains"; an empty topic set means the client sent no
// subscribe frame yet and receives nothing.
func (s Subscription) wants(f Frame) bool {
	if len(s.Topics) == 0 {
		return false
	}
	// A resync applies to every subscribed client regardless of topic or chain:
	// it means the client's picture may have a hole in it.
	if f.T == TopicResync {
		return true
	}
	if !s.Topics[f.T] {
		return false
	}
	if len(s.Chains) > 0 && f.Chain != "" && !s.Chains[f.Chain] {
		return false
	}
	return true
}

// Client is one connected consumer. The transport (the WebSocket itself) lives
// in the api package; this type owns only the queue and the subscription, so
// the hub has no opinion about how frames reach the wire.
type Client struct {
	id  string
	out chan Frame

	mu     sync.RWMutex
	sub    Subscription
	closed bool
	// resync is set when the client was dropped for being too slow, so the
	// writer can tell it to refetch instead of silently vanishing.
	resync bool
}

func NewClient(id string, queue int) *Client {
	if queue <= 0 {
		queue = 512
	}
	return &Client{
		id:  id,
		out: make(chan Frame, queue),
		sub: Subscription{Topics: map[string]bool{}, Chains: map[chain.ID]bool{}},
	}
}

func (c *Client) ID() string { return c.id }

// Out is the queue the transport drains.
func (c *Client) Out() <-chan Frame { return c.out }

// Subscribe replaces the client's subscription.
func (c *Client) Subscribe(topics []string, chains []chain.ID) {
	t := make(map[string]bool, len(topics))
	for _, s := range topics {
		t[s] = true
	}
	ch := make(map[chain.ID]bool, len(chains))
	for _, s := range chains {
		ch[s] = true
	}
	c.mu.Lock()
	c.sub = Subscription{Topics: t, Chains: ch}
	c.mu.Unlock()
}

func (c *Client) Subscription() Subscription {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sub
}

func (c *Client) wants(f Frame) bool {
	c.mu.RLock()
	sub := c.sub
	closed := c.closed
	c.mu.RUnlock()
	if closed {
		return false
	}
	return sub.wants(f)
}

// trySend never blocks. A false return means the queue is full and the caller
// should drop the client.
func (c *Client) trySend(f Frame) bool {
	c.mu.RLock()
	closed := c.closed
	c.mu.RUnlock()
	if closed {
		return true // already gone; not a backpressure failure
	}

	select {
	case c.out <- f:
		return true
	default:
		return false
	}
}

// Send is used for the initial replay, where blocking briefly is acceptable
// because nothing else is being written to this client yet.
func (c *Client) Send(f Frame) bool { return c.trySend(f) }

func (c *Client) closeWithResync() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.resync = true
	c.closed = true
	c.mu.Unlock()
	close(c.out)
}

func (c *Client) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	close(c.out)
}

// NeedsResync reports whether this client was dropped for lag.
func (c *Client) NeedsResync() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.resync
}
