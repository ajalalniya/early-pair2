// Package rpcpool owns every outbound RPC call. Business logic never dials a
// node directly: it asks the pool, which picks a healthy endpoint, applies that
// endpoint's rate budget, hedges when the first is slow, records health, and
// keeps the public-provider failure modes (429s, capped log ranges, stale
// heads, dropped sockets) contained in one place.
package rpcpool

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/rpc"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/config"
	"github.com/earlypair/backend/internal/obs"
)

// Pool is the endpoint registry for every configured chain.
type Pool struct {
	policy config.RPCPolicy
	log    *slog.Logger

	mu        sync.RWMutex
	byChain   map[chain.ID][]*Endpoint
	byID      map[string]*Endpoint
	fleetHead map[chain.ID]uint64
	pollMS    map[chain.ID]int
}

// New builds the pool. Endpoints belonging to chains that are not enabled are
// skipped so a disabled chain costs nothing.
func New(policy config.RPCPolicy, endpoints []config.Endpoint, reg *chain.Registry, log *slog.Logger) (*Pool, error) {
	p := &Pool{
		policy:    policy,
		log:       log.With("component", "rpcpool"),
		byChain:   map[chain.ID][]*Endpoint{},
		byID:      map[string]*Endpoint{},
		fleetHead: map[chain.ID]uint64{},
		pollMS:    map[chain.ID]int{},
	}
	for _, e := range endpoints {
		if e.Disabled {
			continue
		}
		ch, ok := reg.Get(e.ChainID)
		if !ok {
			continue // chain disabled; not an error
		}
		ep := newEndpoint(e, ch, policy)
		p.byChain[e.ChainID] = append(p.byChain[e.ChainID], ep)
		p.byID[e.ID] = ep
		p.pollMS[e.ChainID] = int(ch.PollInterval().Milliseconds())
	}
	if len(p.byID) == 0 {
		return nil, fmt.Errorf("rpcpool: no usable endpoints configured")
	}
	return p, nil
}

// Add registers an endpoint at runtime, backing POST /rpc/endpoints.
func (p *Pool) Add(e config.Endpoint, ch chain.Chain) (*Endpoint, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.byID[e.ID]; exists {
		return nil, fmt.Errorf("rpcpool: endpoint %q already registered", e.ID)
	}
	ep := newEndpoint(e, ch, p.policy)
	p.byChain[e.ChainID] = append(p.byChain[e.ChainID], ep)
	p.byID[e.ID] = ep
	p.log.Info("endpoint added", "endpoint", ep.ID, "chain", ep.ChainID,
		"transport", ep.Transport, "url", ep.Masked)
	return ep, nil
}

func (p *Pool) ByID(id string) (*Endpoint, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byID[id]
	return e, ok
}

// Endpoints returns every endpoint for a chain with the given transport,
// best-first. Unhealthy endpoints are excluded, not merely deprioritised.
func (p *Pool) Endpoints(chainID chain.ID, transport chain.Transport) []*Endpoint {
	p.mu.RLock()
	all := append([]*Endpoint(nil), p.byChain[chainID]...)
	p.mu.RUnlock()

	now := time.Now()
	var out []*Endpoint
	for _, e := range all {
		if e.Transport != transport {
			continue
		}
		if transport == chain.WS {
			if demoted, _ := e.wsDemoted(now); demoted {
				continue
			}
		}
		if !e.available(now) {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score(now) > out[j].score(now) })
	return out
}

// ChainRate is the combined request rate, in rate-limit tokens per second, of
// a chain's usable HTTP endpoints, as currently throttled.
func (p *Pool) ChainRate(chainID chain.ID) float64 {
	var r float64
	for _, e := range p.Endpoints(chainID, chain.HTTP) {
		e.mu.Lock()
		r += e.curRate
		e.mu.Unlock()
	}
	return r
}

// AllEndpoints returns every endpoint for a chain regardless of health, for the
// health panel and for the subscription manager (which must keep retrying a
// socket that is currently down).
func (p *Pool) AllEndpoints(chainID chain.ID) []*Endpoint {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]*Endpoint(nil), p.byChain[chainID]...)
}

// Call runs a JSON-RPC method on the single best HTTP endpoint, retrying on the
// next endpoint when the failure is transient.
func (p *Pool) Call(ctx context.Context, chainID chain.ID, class CostClass, out any, method string, args ...any) error {
	eps := p.Endpoints(chainID, chain.HTTP)
	if len(eps) == 0 {
		return fmt.Errorf("%w (chain %s)", ErrNoEndpoint, chainID)
	}
	var lastErr error
	for _, ep := range eps {
		err := p.CallOn(ctx, ep, class, out, method, args...)
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return err
		}
		if !IsRetryable(err) {
			return err
		}
	}
	return lastErr
}

// CallHedged sends to the best endpoint and, if it has not answered within the
// hedge delay, races a second. The first valid answer wins. This is the reason
// polling beats subscriptions on public infrastructure: a slow provider costs
// milliseconds, not a missed block.
func (p *Pool) CallHedged(ctx context.Context, chainID chain.ID, class CostClass, out any, method string, args ...any) error {
	eps := p.Endpoints(chainID, chain.HTTP)
	if len(eps) == 0 {
		return fmt.Errorf("%w (chain %s)", ErrNoEndpoint, chainID)
	}
	maxHedges := p.policy.MaxHedges
	if maxHedges < 1 {
		maxHedges = 1
	}
	if len(eps) > maxHedges {
		eps = eps[:maxHedges]
	}
	if len(eps) == 1 {
		return p.CallOn(ctx, eps[0], class, out, method, args...)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // cancels the losers as soon as a winner returns

	type result struct {
		val any
		err error
	}
	results := make(chan result, len(eps))
	// Each racer decodes into its own value so a loser cannot scribble on the
	// caller's out pointer after we have already returned a winner.
	launch := func(ep *Endpoint) {
		b := newBox(out)
		err := p.CallOn(ctx, ep, class, b.ptr(), method, args...)
		results <- result{val: b.value(), err: err}
	}

	hedgeAfter := p.policy.HedgeAfter
	if hedgeAfter <= 0 {
		hedgeAfter = 400 * time.Millisecond
	}
	timer := time.NewTimer(hedgeAfter)
	defer timer.Stop()

	next, inflight := 0, 0
	launchNext := func() {
		if next < len(eps) {
			go launch(eps[next])
			next++
			inflight++
		}
	}
	launchNext()

	var lastErr error
	for inflight > 0 {
		select {
		case <-timer.C:
			// The leader is slow: race the next-best rather than wait it out.
			launchNext()
			timer.Reset(hedgeAfter)
		case r := <-results:
			inflight--
			if r.err == nil {
				assign(out, r.val)
				return nil
			}
			lastErr = r.err
			launchNext()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return lastErr
}

// CallOn runs a method on a specific endpoint, applying its rate limit, timeout
// and health accounting. Everything else routes through here.
func (p *Pool) CallOn(ctx context.Context, ep *Endpoint, class CostClass, out any, method string, args ...any) error {
	if err := ep.wait(ctx, class); err != nil {
		return err
	}
	client, err := ep.dial(ctx)
	if err != nil {
		return fmt.Errorf("endpoint %s: %w", ep.ID, err)
	}

	timeout := p.policy.RequestTimeout
	if timeout <= 0 {
		timeout = 12 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	err = client.CallContext(callCtx, out, method, args...)
	dur := time.Since(start)

	if err != nil {
		if limited, retry := IsRateLimited(err); limited {
			ep.rateLimited(retry)
			p.log.Warn("provider rate limited us", "endpoint", ep.ID, "method", method,
				"url", ep.Masked, "retryAfter", retry)
		}
		// A revert, a refused query shape and a "range too large" are all real
		// answers from a healthy node. Counting them as failures would let the
		// honeypot probe's expected reverts, or a catch-up's archive refusals,
		// trip the breaker and bar the endpoint from calls it serves perfectly —
		// including the head checks every chain depends on.
		if IsExecutionReverted(err) || IsCapabilityRefused(err) || IsRangeTooLarge(err) {
			ep.recordSuccess(method, dur)
		} else {
			ep.recordFailure(err)
		}
		obs.RPCRequests.WithLabelValues(ep.ID, method, "error").Inc()
		return fmt.Errorf("endpoint %s: %s: %w", ep.ID, method, err)
	}

	ep.recordSuccess(method, dur)
	return nil
}

// BatchOn sends several calls to one endpoint in a single request. Each call is
// charged against the endpoint's rate budget individually, since providers
// meter batched calls one by one.
func (p *Pool) BatchOn(ctx context.Context, ep *Endpoint, class CostClass, elems []rpc.BatchElem) error {
	for range elems {
		if err := ep.wait(ctx, class); err != nil {
			return err
		}
	}
	client, err := ep.dial(ctx)
	if err != nil {
		return fmt.Errorf("endpoint %s: %w", ep.ID, err)
	}
	timeout := p.policy.RequestTimeout
	if timeout <= 0 {
		timeout = 12 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	err = client.BatchCallContext(callCtx, elems)
	if err != nil {
		if limited, retry := IsRateLimited(err); limited {
			ep.rateLimited(retry)
		}
		ep.recordFailure(err)
		obs.RPCRequests.WithLabelValues(ep.ID, "batch", "error").Inc()
		return fmt.Errorf("endpoint %s: batch: %w", ep.ID, err)
	}
	ep.recordSuccess("batch", time.Since(start))
	return nil
}

// NoteHead records a head observation for an endpoint (from a poll or a socket)
// and refreshes the chain's quorum head.
func (p *Pool) NoteHead(ep *Endpoint, block uint64) {
	ep.noteHead(block)
	p.recomputeFleetHead(ep.ChainID)
}

// recomputeFleetHead takes the quorum view: the max head among endpoints that
// are not absurdly ahead of the median. A single public node occasionally
// reports a head from the future; trusting it would park the cursor past the
// real tip and silently stop the feed.
func (p *Pool) recomputeFleetHead(chainID chain.ID) {
	eps := p.AllEndpoints(chainID)
	heads := make([]uint64, 0, len(eps))
	for _, e := range eps {
		e.mu.Lock()
		h, at := e.head, e.headAt
		e.mu.Unlock()
		// Ignore stale observations: an endpoint that stopped reporting must not
		// pin the fleet head at an old value.
		if h > 0 && time.Since(at) < 2*time.Minute {
			heads = append(heads, h)
		}
	}
	if len(heads) == 0 {
		return
	}
	sort.Slice(heads, func(i, j int) bool { return heads[i] < heads[j] })
	median := heads[len(heads)/2]

	best := median
	for _, h := range heads {
		if h <= best {
			continue
		}
		// Accept a higher head only if it is plausibly ahead, not wildly so.
		if h-median <= p.policy.HeadLagEvictBlocks*4 {
			best = h
		}
	}

	p.mu.Lock()
	p.fleetHead[chainID] = best
	p.mu.Unlock()

	for _, e := range eps {
		e.noteLag(best)
	}
}

// observeHead records a head seen by a call whose winning endpoint is unknown
// (a hedged request). It only ever moves the chain head forward; the quorum
// logic in recomputeFleetHead still governs what the per-endpoint observations
// agree on.
func (p *Pool) observeHead(chainID chain.ID, block uint64) {
	if block == 0 {
		return
	}
	p.mu.Lock()
	if block > p.fleetHead[chainID] {
		p.fleetHead[chainID] = block
	}
	head := p.fleetHead[chainID]
	p.mu.Unlock()

	for _, e := range p.AllEndpoints(chainID) {
		e.noteLag(head)
	}
}

// FleetHead returns the last computed quorum head for a chain.
func (p *Pool) FleetHead(chainID chain.ID) uint64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.fleetHead[chainID]
}

// Status renders every endpoint for the health API, ordered by chain then
// transport so the panel is stable across refreshes.
func (p *Pool) Status() []Status {
	p.mu.RLock()
	chains := make([]chain.ID, 0, len(p.byChain))
	for id := range p.byChain {
		chains = append(chains, id)
	}
	pollMS := map[chain.ID]int{}
	for k, v := range p.pollMS {
		pollMS[k] = v
	}
	p.mu.RUnlock()

	sort.Slice(chains, func(i, j int) bool { return chains[i] < chains[j] })

	var out []Status
	for _, id := range chains {
		head := p.FleetHead(id)
		eps := p.AllEndpoints(id)
		sort.SliceStable(eps, func(i, j int) bool {
			if eps[i].Transport != eps[j].Transport {
				return eps[i].Transport == chain.WS // sockets first, as in the UI
			}
			return eps[i].ID < eps[j].ID
		})
		for _, e := range eps {
			out = append(out, e.Status(head, pollMS[id]))
		}
	}
	return out
}

// Run is the janitor: it recovers rate limits toward baseline and sweeps every
// endpoint's head so the panel and the lag eviction have fresh data even for
// endpoints the selector is not currently choosing.
func (p *Pool) Run(ctx context.Context) error {
	const step = 10 * time.Second
	recover := time.NewTicker(step)
	defer recover.Stop()
	sweep := time.NewTicker(5 * time.Second)
	defer sweep.Stop()

	for {
		select {
		case <-ctx.Done():
			p.closeAll()
			return ctx.Err()
		case <-recover.C:
			p.mu.RLock()
			eps := make([]*Endpoint, 0, len(p.byID))
			for _, e := range p.byID {
				eps = append(eps, e)
			}
			p.mu.RUnlock()
			for _, e := range eps {
				e.recoverRate(step)
			}
		case <-sweep.C:
			p.sweepHeads(ctx)
		}
	}
}

// sweepHeads polls eth_blockNumber on every HTTP endpoint, including ones the
// selector is avoiding. This is what makes "1 block behind" in the health panel
// real rather than inferred, and it is how an evicted endpoint earns its way
// back into rotation.
func (p *Pool) sweepHeads(ctx context.Context) {
	p.mu.RLock()
	chains := make([]chain.ID, 0, len(p.byChain))
	for id := range p.byChain {
		chains = append(chains, id)
	}
	p.mu.RUnlock()

	var wg sync.WaitGroup
	for _, id := range chains {
		for _, ep := range p.AllEndpoints(id) {
			if ep.Transport != chain.HTTP {
				continue
			}
			wg.Add(1)
			go func(ep *Endpoint) {
				defer wg.Done()
				cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
				defer cancel()
				var head hexUint64
				if err := p.CallOn(cctx, ep, ClassCheap, &head, "eth_blockNumber"); err != nil {
					return
				}
				ep.noteHead(uint64(head))
			}(ep)
		}
	}
	wg.Wait()

	for _, id := range chains {
		p.recomputeFleetHead(id)
	}
}

// PingEndpoint measures one endpoint directly, bypassing selection. It backs
// the health panel's Ping action, where the point is to test the endpoint the
// operator clicked rather than whichever one the scorer currently prefers.
func (p *Pool) PingEndpoint(ctx context.Context, ep *Endpoint) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	if ep.Transport != chain.HTTP {
		// A socket cannot be probed without disturbing the supervisor that owns
		// it, so report what the supervisor last saw rather than racing it into
		// holding two connections.
		ep.mu.Lock()
		head, at := ep.head, ep.headAt
		ep.mu.Unlock()
		if head == 0 || time.Since(at) > 2*time.Minute {
			return 0, fmt.Errorf("rpcpool: socket %s has no recent head", ep.ID)
		}
		return head, nil
	}

	var head hexUint64
	if err := p.CallOn(ctx, ep, ClassCheap, &head, "eth_blockNumber"); err != nil {
		return 0, err
	}
	ep.noteHead(uint64(head))
	p.recomputeFleetHead(ep.ChainID)
	return uint64(head), nil
}

func (p *Pool) closeAll() {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, e := range p.byID {
		e.close()
	}
}
