// Package ingest detects newly deployed pools.
//
// The design rule this package exists to enforce: HTTP eth_getLogs polling on a
// persisted block cursor is the source of truth, and WebSocket subscriptions
// are only a doorbell that wakes the poll loop early. Nothing here writes a
// pool to the database because a socket said so.
//
// The reason is specific to running on free public endpoints. eth_subscribe has
// no replay: when a public socket drops — and they drop constantly, from
// provider restarts, idle timeouts, connection caps and load-balancer rotation
// — every log emitted during the gap is gone, with no way to discover what was
// missed. A missed PairCreated is a missed product event. eth_getLogs against a
// persisted cursor has the opposite property: a crash, a deploy or a partition
// costs latency, and the cursor picks up exactly where it left off.
//
// What the socket buys is the few hundred milliseconds between a block landing
// and the next poll tick. If every socket in the fleet dies, detection gets
// about a second slower and nothing else changes.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/config"
	"github.com/earlypair/backend/internal/obs"
	"github.com/earlypair/backend/internal/rpcpool"
	"github.com/earlypair/backend/internal/store"
)

// Sink receives everything the ingester discovers. The stream hub and the
// enrichment queue both sit behind it.
type Sink interface {
	PoolDetected(ctx context.Context, p chain.Pool)
	PoolStateChanged(ctx context.Context, p chain.Pool)
	HeadAdvanced(ctx context.Context, chainID chain.ID, block uint64, at time.Time)
	TopicMatched(ctx context.Context, m store.TopicMatch)
	// PoolMarketUpdated carries the market tracker's refreshed figures.
	PoolMarketUpdated(ctx context.Context, p chain.Pool)
	// PoolRemoved tells clients a pair failed screening and left the feed.
	PoolRemoved(ctx context.Context, p chain.Pool)
}

// liveWindow is how old a pool's block can be and still count as a live
// detection. Generous enough to cover a slow sweep behind a throttled
// provider; far shorter than any restart catch-up.
const liveWindow = 5 * time.Minute

// Ingester runs one chain. Chains never share goroutines, rate budgets or
// failure domains: a backlog on BSC must not stall Base.
type Ingester struct {
	ch    chain.Chain
	cfg   config.Ingest
	pool  *rpcpool.Pool
	evm   *rpcpool.EVM
	store Store
	sink  Sink
	log   *slog.Logger

	factories      chain.FactoryIndex
	creationTopics []common.Hash

	mu          sync.RWMutex
	watchTopics []common.Hash

	// nativePricePeers are chains sharing this chain's native asset, used to
	// price it when this chain has no reference pool of its own.
	nativePricePeers []chain.ID
}

// SetNativePricePeers names chains whose native-token price also prices this
// chain's native token (the same asset, e.g. ETH on an L2).
func (i *Ingester) SetNativePricePeers(ids []chain.ID) { i.nativePricePeers = ids }

func New(ch chain.Chain, cfg config.Ingest, pool *rpcpool.Pool, st Store, sink Sink, log *slog.Logger) *Ingester {
	return &Ingester{
		ch:             ch,
		cfg:            cfg,
		pool:           pool,
		evm:            pool.EVM(ch.ID),
		store:          st,
		sink:           sink,
		log:            log.With("component", "ingest", "chain", ch.ID),
		factories:      chain.NewFactoryIndex(ch.Factories),
		creationTopics: chain.CreationTopics(ch.Factories),
	}
}

// Run owns the chain until the context is cancelled.
func (i *Ingester) Run(ctx context.Context) error {
	if i.ch.Kind != chain.KindEVM {
		// Solana and SUI follow the same poll-is-truth doctrine with different
		// primitives (getSignaturesForAddress / suix_queryEvents cursors); those
		// ingesters are not implemented yet, and a chain configured for them is
		// skipped loudly rather than silently producing an empty feed.
		i.log.Warn("no ingester implemented for this chain kind; skipping",
			"kind", i.ch.Kind)
		<-ctx.Done()
		return ctx.Err()
	}
	if len(i.ch.Factories) == 0 {
		// A chain can be configured for RPC, balances and signing before its DEX
		// factories are known. There is nothing to sweep, and an address-less
		// getLogs would pull every log on the chain, so idle. /chains reports
		// ingesting=false for this chain, so the UI does not imply a live feed.
		i.log.Warn("no factories configured for this chain; pool discovery is off")
		<-ctx.Done()
		return ctx.Err()
	}
	if len(i.creationTopics) == 0 {
		return fmt.Errorf("ingest: chain %s has no decodable factories configured", i.ch.ID)
	}

	cursor, err := i.resolveStart(ctx)
	if err != nil {
		return err
	}
	i.log.Info("ingest starting", "cursor", cursor.LastBlock,
		"factories", len(i.factories), "creationTopics", len(i.creationTopics))
	i.checkBlockTime(ctx)

	// The doorbell. Advisory only — see the package comment.
	bell := i.pool.StartSubscriptions(ctx, i.ch, rpcpool.SubSpec{
		// Addressed for the same reason the sweep is: some endpoints refuse an
		// address-less logs subscription, and heads-only doorbells are slower.
		Addresses: i.factories.Addresses(),
		Topics:    [][]common.Hash{i.creationTopics},
	})

	go i.refreshLoop(ctx)
	go i.confirmLoop(ctx)
	go i.trackLoop(ctx)

	tick := time.NewTicker(i.ch.PollInterval())
	defer tick.Stop()

	debounce := i.cfg.DoorbellDebounce
	if debounce <= 0 {
		debounce = 150 * time.Millisecond
	}
	var pending bool
	settle := time.NewTimer(time.Hour)
	settle.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-tick.C:
			i.advanceLogged(ctx)

		case <-bell.C:
			// Coalesce a burst of hints (several sockets, heads and logs all
			// firing for the same block) into one sweep.
			if !pending {
				pending = true
				settle.Reset(debounce)
			}

		case <-settle.C:
			pending = false
			i.advanceLogged(ctx)
		}
	}
}

// checkBlockTime measures the chain's real block time and warns when the
// configured value is off. The configured value turns every wall-clock window
// into a block count, so drift is silent data corruption: BSC and Arbitrum
// were configured at 3s and 1s against real times of 0.45s and 0.25s, and
// their "last hour" trader counts covered nine and fifteen minutes. Chains
// change block times (BSC did, twice), so this runs at every start.
func (i *Ingester) checkBlockTime(ctx context.Context) {
	measured, err := i.measureBlockTime(ctx)
	if err != nil {
		i.log.Debug("could not measure block time", "error", err)
		return
	}
	configured := i.ch.ExpectedBlockTime()
	ratio := float64(configured) / float64(measured)
	if ratio > 1.5 || ratio < 1/1.5 {
		i.log.Warn("configured blockTime does not match the chain; time windows are wrong until it is fixed",
			"configured", configured, "measured", measured.Round(time.Millisecond))
	}
}

func (i *Ingester) measureBlockTime(ctx context.Context) (time.Duration, error) {
	const span = 2000
	head, err := i.evm.BlockNumber(ctx)
	if err != nil {
		return 0, err
	}
	if head <= span {
		return 0, fmt.Errorf("chain too short to measure")
	}
	newer, err := i.evm.HeaderByNumber(ctx, head)
	if err != nil {
		return 0, err
	}
	older, err := i.evm.HeaderByNumber(ctx, head-span)
	if err != nil {
		return 0, err
	}
	if newer.Timestamp <= older.Timestamp {
		return 0, fmt.Errorf("non-increasing timestamps")
	}
	return time.Duration(newer.Timestamp-older.Timestamp) * time.Second / span, nil
}

func (i *Ingester) advanceLogged(ctx context.Context) {
	if err := i.advance(ctx); err != nil && !errors.Is(err, context.Canceled) {
		i.log.Warn("sweep failed; will retry on next tick", "error", err)
	}
}

// resolveStart decides where a cold or stale cursor begins.
func (i *Ingester) resolveStart(ctx context.Context) (store.Cursor, error) {
	cur, ok, err := i.store.GetCursor(ctx, i.ch.ID)
	if err != nil {
		return store.Cursor{}, err
	}

	head, err := i.evm.BlockNumber(ctx)
	if err != nil {
		return store.Cursor{}, fmt.Errorf("ingest: cannot reach %s to establish head: %w", i.ch.ID, err)
	}

	switch {
	case !ok:
		start := i.ch.StartBlock
		if start == 0 || start > head {
			start = head
		}
		cur = store.Cursor{LastBlock: start}
		i.log.Info("cold start", "from", start, "head", head)

	case head > cur.LastBlock && !i.cfg.ResumeOnStart:
		// The feed is for pairs as they launch. Pairs created while the indexer
		// was stopped are not new by the time it is back, and sweeping for them
		// spends the rate budget new pairs need — on Robinhood's single endpoint
		// a restart spent tens of minutes catching up while live pairs waited.
		// So a start begins at the head. A gap that opens while running (an RPC
		// outage) is still swept; only the downtime before a start is skipped.
		i.log.Info("starting at head; blocks missed while stopped are skipped",
			"cursor", cur.LastBlock, "head", head, "skipped", head-cur.LastBlock)
		cur = store.Cursor{LastBlock: head}

	case head > cur.LastBlock && head-cur.LastBlock > i.cfg.MaxBackfillBlocks:
		// A cursor this stale would burn the entire rate budget catching up and
		// then flood the feed with pools nobody wants to see. Jump to head and
		// say so, rather than quietly grinding for hours.
		i.log.Warn("cursor too far behind; skipping ahead to head",
			"cursor", cur.LastBlock, "head", head, "gap", head-cur.LastBlock,
			"maxBackfill", i.cfg.MaxBackfillBlocks)
		cur = store.Cursor{LastBlock: head}
	}

	if err := i.store.SetCursor(ctx, i.ch.ID, cur); err != nil {
		return store.Cursor{}, err
	}
	return cur, nil
}

// advance moves the cursor to head, one bounded chunk at a time.
func (i *Ingester) advance(ctx context.Context) error {
	cur, ok, err := i.store.GetCursor(ctx, i.ch.ID)
	if err != nil {
		return err
	}
	if !ok {
		cur, err = i.resolveStart(ctx)
		if err != nil {
			return err
		}
	}

	head := i.pool.FleetHead(i.ch.ID)
	if head == 0 {
		head, err = i.evm.BlockNumber(ctx)
		if err != nil {
			return err
		}
	}
	if head <= cur.LastBlock {
		obs.CursorLag.WithLabelValues(string(i.ch.ID)).Set(0)
		return nil
	}
	obs.CursorLag.WithLabelValues(string(i.ch.ID)).Set(float64(head - cur.LastBlock))

	// Before reading forward, make sure the block we last processed is still on
	// the canonical chain. If it is not, rewind first — otherwise we would
	// happily append logs from a chain that no longer exists.
	if rewound, err := i.checkReorg(ctx, &cur); err != nil {
		return err
	} else if rewound {
		i.log.Warn("reorg handled; rescanning", "cursor", cur.LastBlock)
	}

	backfill := head-cur.LastBlock > 100

	for from := cur.LastBlock + 1; from <= head; {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		span := i.chunk(backfill, head-from)
		if span == 0 {
			return fmt.Errorf("ingest: %w for %s", rpcpool.ErrNoEndpoint, i.ch.ID)
		}
		to := from + span - 1
		if to > head {
			to = head
		}

		logs, err := i.sweep(ctx, from, to)
		if err != nil {
			return err
		}

		if err := i.process(ctx, logs, from, to); err != nil {
			return err
		}

		// Persist after every chunk. If the process dies mid-catch-up, the work
		// already done is not repeated — and if it is repeated, the dedupe index
		// makes that free.
		cur.LastBlock = to
		if hdr, err := i.evm.HeaderByNumber(ctx, to); err == nil {
			cur.LastHash = hdr.Hash.Hex()
			_ = i.store.PutBlockHash(ctx, i.ch.ID, store.Block{
				Number: hdr.Number, Hash: hdr.Hash.Hex(),
				ParentHash: hdr.ParentHash.Hex(), Time: time.Unix(int64(hdr.Timestamp), 0).UTC(),
			})
			i.sink.HeadAdvanced(ctx, i.ch.ID, hdr.Number, time.Unix(int64(hdr.Timestamp), 0).UTC())
		}
		if err := i.store.SetCursor(ctx, i.ch.ID, cur); err != nil {
			return err
		}

		from = to + 1
	}

	obs.CursorLag.WithLabelValues(string(i.ch.ID)).Set(float64(head - cur.LastBlock))
	return nil
}

// chunk decides how many blocks to ask for at once: the proven range of the
// best endpoint that will serve log queries at all. A wide historical catch-up
// is capped further, so a backfill cannot crowd out live ingestion. Zero means
// the chain has no HTTP endpoint to ask.
func (i *Ingester) chunk(backfill bool, depth uint64) uint64 {
	eps := i.pool.Endpoints(i.ch.ID, chain.HTTP)
	if len(eps) == 0 {
		return 0
	}
	ep := eps[0]
	for _, e := range eps {
		if !e.Refuses(rpcpool.CapLogs, depth) {
			ep = e
			break
		}
	}
	span := ep.LogRange()
	limit := i.cfg.BackfillChunk
	if i.ch.BackfillChunk > 0 {
		limit = i.ch.BackfillChunk
	}
	if backfill && limit > 0 && limit < span {
		span = limit
	}
	if span == 0 {
		span = 1
	}
	return span
}

// errRangeTooLarge tells sweepQuery to bisect. It is internal: callers only
// ever see logs or a real failure.
var errRangeTooLarge = errors.New("ingest: range too large for every endpoint tried")

// sweep reads a block range: pool creations first, then watched topics.
//
// These are two queries, not one, because free endpoints disagree about what
// they will serve. A creation query carries the factory addresses, which every
// provider accepts and which is also the cheaper request, since providers
// index logs by address. Watched topics have to be matched across every
// contract on the chain, so that query carries no address filter — and some
// public endpoints refuse exactly that shape outright ("Please specify an
// address in your request"). Folding both into one address-less query, as this
// once did, let a single such endpoint stall pool detection for a whole chain.
//
// Pool creation is the product; a watched topic is a feature on top of it. So
// the creation query must succeed for the range to count, while a watch query
// that no endpoint will serve is skipped and counted rather than allowed to
// hold the cursor back.
func (i *Ingester) sweep(ctx context.Context, from, to uint64) ([]types.Log, error) {
	logs, err := i.sweepQuery(ctx, from, to, i.factories.Addresses(), [][]common.Hash{i.creationTopics}, rpcpool.CapLogs)
	if err != nil {
		return nil, err
	}

	i.mu.RLock()
	watch := i.watchTopics
	i.mu.RUnlock()
	if len(watch) == 0 {
		return logs, nil
	}

	watched, err := i.sweepQuery(ctx, from, to, nil, [][]common.Hash{watch}, rpcpool.CapLogsNoAddress)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		obs.WatchSweepsSkipped.WithLabelValues(string(i.ch.ID)).Inc()
		i.log.Warn("watched topics could not be swept for this range; pool detection is unaffected",
			"from", from, "to", to, "error", err)
		return logs, nil
	}

	// A watched topic can also be a creation topic — watching PairCreated is
	// the obvious example — so the same log may arrive from both queries.
	seen := make(map[string]bool, len(logs))
	for _, l := range logs {
		seen[logKey(l.TxHash.Hex(), l.Index)] = true
	}
	for _, l := range watched {
		if k := logKey(l.TxHash.Hex(), l.Index); !seen[k] {
			seen[k] = true
			logs = append(logs, l)
		}
	}
	return logs, nil
}

// sweepQuery fetches one query shape across a range, bisecting whenever the
// range is too large for every endpoint willing to serve it. Public endpoints
// disagree about their own limits and change them without notice, so the
// working size is discovered, not assumed.
func (i *Ingester) sweepQuery(ctx context.Context, from, to uint64, addrs []common.Address,
	topics [][]common.Hash, capability rpcpool.Capability) ([]types.Log, error) {

	var out []types.Log
	// A tiny explicit stack beats recursion here: the failure mode we are
	// guarding against is a provider that rejects everything, and a stack makes
	// the bound on attempts obvious.
	type rng struct{ from, to uint64 }
	stack := []rng{{from, to}}

	for len(stack) > 0 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		r := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		logs, err := i.fetchRange(ctx, rpcpool.FilterQuery{
			FromBlock: r.from, ToBlock: r.to,
			Addresses: addrs,
			Topics:    topics,
		}, capability)

		switch {
		case err == nil:
			out = append(out, logs...)
		case errors.Is(err, errRangeTooLarge) && r.from < r.to:
			mid := r.from + (r.to-r.from)/2
			stack = append(stack, rng{mid + 1, r.to}, rng{r.from, mid})
		default:
			return nil, err
		}
	}
	return out, nil
}

// fetchRange runs one query, trying endpoints best-first until one serves it.
//
// Failing over within a single range is what keeps one misbehaving provider
// from stalling a chain. Choosing an endpoint once per tick and failing the
// whole tick on its error meant that an endpoint which answers eth_blockNumber
// perfectly — and so stays top-scored — while refusing every log query was
// asked the same doomed question on every tick, and the healthy endpoints
// behind it were never tried.
func (i *Ingester) fetchRange(ctx context.Context, q rpcpool.FilterQuery, capability rpcpool.Capability) ([]types.Log, error) {
	eps := i.pool.Endpoints(i.ch.ID, chain.HTTP)
	if len(eps) == 0 {
		return nil, fmt.Errorf("ingest: %w for %s", rpcpool.ErrNoEndpoint, i.ch.ID)
	}

	// How far behind the tip this query starts. Archive refusals are about
	// depth: an endpoint that will not serve a catch-up range still serves the
	// tip, so it is only skipped for queries at least this deep.
	var depth uint64
	if head := i.pool.FleetHead(i.ch.ID); head > q.FromBlock {
		depth = head - q.FromBlock
	}

	var lastErr error
	tooLarge := false
	tried := 0

	for _, ep := range eps {
		if ep.Refuses(capability, depth) || (capability != rpcpool.CapLogs && ep.Refuses(rpcpool.CapLogs, depth)) {
			continue
		}
		tried++

		logs, err := i.evm.FilterLogs(ctx, ep, q)
		if err != nil && capability == rpcpool.CapLogs && rpcpool.IsCapabilityRefused(err) && ctx.Err() == nil {
			// Load-balanced public endpoints send each request to a backend with
			// its own history depth, so an archive refusal is often not repeated.
			// One immediate retry is far cheaper than failing the tick.
			logs, err = i.evm.FilterLogs(ctx, ep, q)
		}
		switch {
		case err == nil:
			ep.GrowLogRange()
			return logs, nil

		case ctx.Err() != nil:
			return nil, ctx.Err()

		case rpcpool.IsCapabilityRefused(err):
			if ep.MarkRefused(capability, depth) {
				i.log.Warn("endpoint refuses this log query on its tier; routing around it",
					"endpoint", ep.ID, "capability", capability, "depth", depth, "error", err)
			}
			lastErr = err

		case rpcpool.IsRangeTooLarge(err):
			ep.ShrinkLogRange()
			tooLarge = true
			lastErr = err

		default:
			// Retryable or not, another provider may answer this range; a
			// genuinely malformed query fails on all of them and surfaces below.
			lastErr = err
		}
	}

	if tried == 0 {
		return nil, fmt.Errorf("ingest: no endpoint on %s currently serves %s queries: %w",
			i.ch.ID, capability, rpcpool.ErrNoEndpoint)
	}
	if tooLarge {
		return nil, fmt.Errorf("%w: %v", errRangeTooLarge, lastErr)
	}
	return nil, lastErr
}

// process turns a swept range into pools and topic matches.
func (i *Ingester) process(ctx context.Context, logs []types.Log, from, to uint64) error {
	if len(logs) == 0 {
		return nil
	}

	// Block timestamps are needed for createdAt and for the detection-latency
	// metric. Fetch each distinct block once.
	times := map[uint64]time.Time{}
	blocks := map[uint64]bool{}
	for _, l := range logs {
		blocks[l.BlockNumber] = true
	}
	for n := range blocks {
		if hdr, err := i.evm.HeaderByNumber(ctx, n); err == nil {
			times[n] = time.Unix(int64(hdr.Timestamp), 0).UTC()
		} else {
			// Without a timestamp we still record the pool; using "now" is
			// honest here because the log was swept within a block or two.
			times[n] = time.Now().UTC()
		}
	}

	i.mu.RLock()
	watching := make(map[common.Hash]bool, len(i.watchTopics))
	for _, t := range i.watchTopics {
		watching[t] = true
	}
	i.mu.RUnlock()

	var matches []store.TopicMatch
	creations := 0

	for _, l := range logs {
		if l.Removed {
			continue // the provider is telling us this log is orphaned
		}
		if len(l.Topics) == 0 {
			continue
		}
		ts := times[l.BlockNumber]

		if watching[l.Topics[0]] {
			matches = append(matches, store.TopicMatch{
				Topic0:      l.Topics[0].Hex(),
				ChainID:     i.ch.ID,
				TxHash:      l.TxHash.Hex(),
				LogIndex:    l.Index,
				BlockNumber: l.BlockNumber,
				Time:        ts,
			})
		}

		// Only logs from a factory we registered become pools. The same topic0
		// emitted by an unknown contract is noise we deliberately do not guess at.
		if _, known := i.factories[strings.ToLower(l.Address.Hex())]; !known {
			continue
		}

		p, err := i.poolFromLog(l, ts)
		if err != nil {
			if !errors.Is(err, chain.ErrUnsupportedEvent) {
				i.log.Debug("skipping undecodable creation log",
					"tx", l.TxHash.Hex(), "error", err)
			}
			continue
		}
		// A pool whose block is older than the live window was found by catching
		// up, not as it happened. It is stored and served like any other, but it
		// is not news: announcing hours of history as "new" after a restart would
		// bury whatever is actually new under it.
		p.Backfilled = !ts.IsZero() && time.Since(ts) > liveWindow

		created, err := i.store.InsertPool(ctx, p)
		if err != nil {
			return err
		}
		if !created {
			// Already known. If it was marked reorged and its creating log is
			// back, the pool survived on the new chain: restore it as a state
			// change rather than announcing it as a discovery, so a rewind
			// never produces a duplicate arrival in the feed.
			restored, err := i.store.RestorePool(ctx, i.ch.ID, l.TxHash.Hex(), l.Index)
			if err != nil {
				return err
			}
			if restored {
				p.State = chain.StateUnconfirmed
				i.sink.PoolStateChanged(ctx, p)
			}
			continue
		}

		creations++
		obs.PairsDetected.WithLabelValues(string(i.ch.ID)).Inc()
		// Only live detections say anything about detection latency. One
		// catch-up after downtime would otherwise put hours-long samples into
		// the p95 the alert watches, and it would stay red long after recovery.
		if !ts.IsZero() && !p.Backfilled {
			obs.DetectionLatency.WithLabelValues(string(i.ch.ID)).Observe(time.Since(ts).Seconds())
		}
		i.sink.PoolDetected(ctx, p)
	}

	if len(matches) > 0 {
		if err := i.store.InsertTopicMatches(ctx, matches); err != nil {
			return err
		}
		for _, m := range matches {
			i.sink.TopicMatched(ctx, m)
		}
	}
	if creations > 0 {
		i.log.Info("pools detected", "count", creations, "from", from, "to", to)
	}
	return nil
}

func (i *Ingester) poolFromLog(l types.Log, ts time.Time) (chain.Pool, error) {
	c, err := chain.DecodeCreation(l, i.factories)
	if err != nil {
		return chain.Pool{}, err
	}
	p := chain.Pool{
		ChainID:      i.ch.ID,
		Address:      c.PoolAddress,
		Dex:          c.Dex,
		Model:        c.Model,
		State:        chain.StateUnconfirmed,
		CreatedAt:    ts,
		CreatedBlock: l.BlockNumber,
		CreatedTx:    l.TxHash.Hex(),
		LogIndex:     l.Index,
		FeeTierBps:   c.FeeBps,
		Hooks:        c.Hooks,
		Token0:       &chain.Token{Address: c.Token0},
		Token1:       &chain.Token{Address: c.Token1},
	}
	// Symbols are unknown until enrichment runs; the pair name is filled in then
	// rather than guessed from the address.
	p.Pair = chain.Orient(i.ch, p.Token0, p.Token1).Name()
	if err := p.Validate(); err != nil {
		return chain.Pool{}, err
	}
	return p, nil
}

// checkReorg verifies the cursor block is still canonical and rewinds if not.
func (i *Ingester) checkReorg(ctx context.Context, cur *store.Cursor) (bool, error) {
	if cur.LastHash == "" || cur.LastBlock == 0 {
		return false, nil
	}

	hdr, err := i.evm.HeaderByNumber(ctx, cur.LastBlock)
	if err != nil {
		// Cannot verify right now; do not rewind on a transport failure.
		return false, nil
	}
	if strings.EqualFold(hdr.Hash.Hex(), cur.LastHash) {
		return false, nil
	}

	obs.Reorgs.WithLabelValues(string(i.ch.ID)).Inc()
	i.log.Warn("reorg detected at cursor",
		"block", cur.LastBlock, "stored", cur.LastHash, "canonical", hdr.Hash.Hex())

	// Walk back to the deepest block both sides still agree on.
	//
	// Block hashes are only recorded at chunk boundaries, so most blocks in the
	// window have no stored hash to compare against. A missing hash is not
	// evidence of agreement, so the walk keeps going rather than stopping at
	// the first gap — stopping there was how a reorg deeper than one chunk went
	// unnoticed.
	maxDepth := i.ch.Confirmations * 4
	if maxDepth == 0 {
		maxDepth = 64
	}

	var fork uint64
	agreed := false
	for depth := uint64(0); depth <= maxDepth && depth <= cur.LastBlock; depth++ {
		n := cur.LastBlock - depth
		if n == 0 {
			fork, agreed = 0, true
			break
		}
		stored, ok, err := i.store.GetBlockHash(ctx, i.ch.ID, n)
		if err != nil {
			return false, err
		}
		if !ok {
			continue // no record for this height; keep walking back
		}
		canon, err := i.evm.HeaderByNumber(ctx, n)
		if err != nil {
			return false, nil // cannot verify right now; do not rewind blindly
		}
		if strings.EqualFold(canon.Hash.Hex(), stored) {
			fork, agreed = n, true
			break
		}
	}
	if !agreed {
		// Nothing in the window agrees. Rewind the whole depth: re-reading a
		// range costs a few requests and the dedupe index makes it a no-op,
		// whereas leaving the cursor past a divergence loses pools outright.
		if cur.LastBlock > maxDepth {
			fork = cur.LastBlock - maxDepth
		}
		i.log.Warn("no agreeing block found in the reorg window; rewinding the full depth",
			"depth", maxDepth, "to", fork)
	}

	// Everything above the fork point came from a chain that no longer exists.
	orphans, err := i.store.ReorgPoolsFrom(ctx, i.ch.ID, fork+1)
	if err != nil {
		return false, err
	}
	for _, addr := range orphans {
		i.sink.PoolStateChanged(ctx, chain.Pool{
			ChainID: i.ch.ID, Address: addr, State: chain.StateReorged,
		})
	}
	// The dedupe rows are deliberately left in place. Deleting them so the
	// rescan could re-insert would make every pool in the rewound range look
	// new again, and a conservative rewind covers blocks that never reorged at
	// all — clients would see the same pools announced twice. Instead the
	// rescan restores the rows whose creating log is still there, and anything
	// still marked reorged afterwards is genuinely gone.
	if err := i.store.DeleteBlockHashesFrom(ctx, i.ch.ID, fork+1); err != nil {
		return false, err
	}

	cur.LastBlock = fork
	cur.LastHash = ""
	if err := i.store.SetCursor(ctx, i.ch.ID, *cur); err != nil {
		return false, err
	}
	i.log.Warn("rewound cursor after reorg",
		"to", fork, "orphanedPools", len(orphans))
	return true, nil
}

// confirmLoop re-verifies unconfirmed pools once they are deep enough, then
// flips them to confirmed or reorged. This is what clears the "unconfirmed"
// pulse in the UI.
func (i *Ingester) confirmLoop(ctx context.Context) {
	every := i.cfg.ConfirmCheckEvery
	if every <= 0 {
		every = 15 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := i.confirmPass(ctx); err != nil && !errors.Is(err, context.Canceled) {
				i.log.Debug("confirmation pass failed", "error", err)
			}
		}
	}
}

func (i *Ingester) confirmPass(ctx context.Context) error {
	head := i.pool.FleetHead(i.ch.ID)
	if head <= i.ch.Confirmations {
		return nil
	}
	safe := head - i.ch.Confirmations

	pools, err := i.store.UnconfirmedBelow(ctx, i.ch.ID, safe, 200)
	if err != nil || len(pools) == 0 {
		return err
	}

	// Group into block ranges one getLogs can cover, so one query verifies every
	// pool created in its range. A query per block was affordable at 12s
	// blocks; at Robinhood's 0.1s, with a pool every few seconds, those queries
	// alone outran the endpoint's whole rate budget and starved the sweep.
	span := i.chunk(false, head-pools[0].CreatedBlock)
	if span == 0 {
		return fmt.Errorf("ingest: %w for %s", rpcpool.ErrNoEndpoint, i.ch.ID)
	}
	type group struct {
		from, to uint64
		pools    []chain.Pool
	}
	var groups []group
	for _, p := range pools { // ordered by created_block
		if n := len(groups); n > 0 && p.CreatedBlock < groups[n-1].from+span {
			groups[n-1].to = p.CreatedBlock
			groups[n-1].pools = append(groups[n-1].pools, p)
			continue
		}
		groups = append(groups, group{from: p.CreatedBlock, to: p.CreatedBlock, pools: []chain.Pool{p}})
	}
	const maxQueriesPerPass = 5
	if len(groups) > maxQueriesPerPass {
		groups = groups[:maxQueriesPerPass]
	}

	for _, g := range groups {
		logs, err := i.sweepQuery(ctx, g.from, g.to, i.factories.Addresses(),
			[][]common.Hash{i.creationTopics}, rpcpool.CapLogs)
		if err != nil {
			return err // transient: leave them unconfirmed and try again
		}

		present := map[string]bool{}
		for _, l := range logs {
			if l.Removed {
				continue
			}
			present[logKey(l.TxHash.Hex(), l.Index)] = true
		}

		for _, p := range g.pools {
			state := chain.StateConfirmed
			if !present[logKey(p.CreatedTx, p.LogIndex)] {
				// The creating log is gone from the canonical chain.
				state = chain.StateReorged
				obs.Reorgs.WithLabelValues(string(i.ch.ID)).Inc()
				i.log.Warn("pool vanished on re-verification; marking reorged",
					"pool", p.Address, "block", p.CreatedBlock, "tx", p.CreatedTx)
			}
			if err := i.store.SetPoolState(ctx, i.ch.ID, p.Address, state); err != nil {
				return err
			}
			p.State = state
			i.sink.PoolStateChanged(ctx, p)
		}
	}

	// Block hashes deeper than the confirmation window can never be rewound to.
	if safe > i.ch.Confirmations*4 {
		_ = i.store.PruneBlockHashes(ctx, i.ch.ID, safe-i.ch.Confirmations*4)
	}
	return nil
}

func logKey(tx string, index uint) string {
	return strings.ToLower(tx) + ":" + fmt.Sprint(index)
}

// refreshLoop keeps the watched-topic set current without putting a database
// query on the sweep's hot path.
func (i *Ingester) refreshLoop(ctx context.Context) {
	i.refreshWatchTopics(ctx)
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			i.refreshWatchTopics(ctx)
		}
	}
}

func (i *Ingester) refreshWatchTopics(ctx context.Context) {
	topics, err := i.store.ActiveTopics(ctx, i.ch.ID)
	if err != nil {
		i.log.Debug("could not refresh watched topics", "error", err)
		return
	}
	hashes := make([]common.Hash, 0, len(topics))
	for _, t := range topics {
		hashes = append(hashes, common.HexToHash(t))
	}
	i.mu.Lock()
	i.watchTopics = hashes
	i.mu.Unlock()
}
