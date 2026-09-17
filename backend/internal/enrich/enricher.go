// Package enrich fills in everything a freshly detected pool does not yet know
// about itself: token identities, liquidity, trader counts, holder
// concentration and the safety probes that feed the risk score.
//
// The governing rule is that enrichment never blocks the feed. A pool appears
// the moment its creating log is decoded, with every derived field null, and
// those fields fill in as they are established. A null reaches the API as null
// and renders as an em dash — it is never replaced with a plausible-looking
// number, because the user sizes a position against what we show them.
package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"golang.org/x/sync/errgroup"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/config"
	"github.com/earlypair/backend/internal/obs"
	"github.com/earlypair/backend/internal/risk"
	"github.com/earlypair/backend/internal/rpcpool"
	"github.com/earlypair/backend/internal/store"
)

// Sink receives enriched pools for publication.
type Sink interface {
	PoolEnriched(ctx context.Context, p chain.Pool)
}

type Enricher struct {
	cfg    config.Ingest
	reg    *chain.Registry
	pool   *rpcpool.Pool
	store  *store.Store
	scorer *risk.Scorer
	sink   Sink
	log    *slog.Logger

	explorers map[string]config.Explorer
	http      *http.Client

	priceMu sync.RWMutex
	native  map[chain.ID]float64 // native token price in USD, from a reference pool
}

// decodeJSON is a small indirection so the explorer probe can be read without
// an encoding/json import bleeding into every file in the package.
func decodeJSON(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) }

func New(cfg config.Ingest, reg *chain.Registry, pool *rpcpool.Pool, st *store.Store,
	scorer *risk.Scorer, explorers map[string]config.Explorer, sink Sink, log *slog.Logger) *Enricher {
	if explorers == nil {
		explorers = map[string]config.Explorer{}
	}
	return &Enricher{
		cfg: cfg, reg: reg, pool: pool, store: st, scorer: scorer, sink: sink,
		explorers: explorers,
		http:      &http.Client{Timeout: 10 * time.Second},
		log:       log.With("component", "enrich"),
		native:    map[chain.ID]float64{},
	}
}

// Run starts the worker pool and the price refresher.
func (e *Enricher) Run(ctx context.Context) error {
	workers := e.cfg.EnrichWorkers
	if workers <= 0 {
		workers = 8
	}
	// The queue is deliberately as short as the worker pool. Priority lives in
	// the database query (newest pools first), and a long in-memory queue would
	// defeat it: work queued seconds ago would sit in front of a pool created
	// just now. See feed.
	queue := make(chan chain.Pool, workers)
	sched := newSchedule(workers)

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return e.refreshPrices(ctx) })
	g.Go(func() error { return e.feed(ctx, queue, sched) })

	for w := 0; w < workers; w++ {
		g.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case p, ok := <-queue:
					if !ok {
						return nil
					}
					err := e.enrich(ctx, p)
					sched.done(p)
					if err != nil && !errors.Is(err, context.Canceled) {
						e.log.Debug("enrichment failed; will retry on the next pass",
							"pool", p.Key(), "error", err)
					}
				}
			}
		})
	}
	return g.Wait()
}

// schedule tracks work that is queued or running, so a pool is never queued
// twice and no chain holds more than its share of the workers.
//
// Both were real failures. The feeder used to re-queue every pool still
// needing enrichment on each tick into a 4,096-slot first-in-first-out queue.
// Pools not yet done were queued again and again, the queue filled with those
// duplicates, and a pool created a minute ago sat behind thousands of them — so
// new pools, the ones users are looking at, were never reached. And a chain on
// a single rate-limited endpoint could occupy every worker for the length of a
// pass each, stalling enrichment on every other chain.
type schedule struct {
	mu       sync.Mutex
	inFlight map[string]bool
	perChain map[chain.ID]int
	chainCap int
}

func newSchedule(workers int) *schedule {
	limit := workers / 2
	if limit < 2 {
		limit = 2
	}
	return &schedule{inFlight: map[string]bool{}, perChain: map[chain.ID]int{}, chainCap: limit}
}

// claim reports whether a pool may be queued now, and reserves it if so.
func (s *schedule) claim(p chain.Pool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight[p.Key()] || s.perChain[p.ChainID] >= s.chainCap {
		return false
	}
	s.inFlight[p.Key()] = true
	s.perChain[p.ChainID]++
	return true
}

// release undoes a claim whose pool could not be queued after all.
func (s *schedule) release(p chain.Pool) { s.done(p) }

func (s *schedule) done(p chain.Pool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight[p.Key()] {
		delete(s.inFlight, p.Key())
		s.perChain[p.ChainID]--
	}
}

// feed pulls work from the database rather than from an in-memory channel fed
// by the ingester. That way a restart resumes enrichment for everything still
// stale, instead of losing whatever was queued.
//
// It runs every second, takes the newest pools needing work, and offers each to
// the short queue only if it is not already in flight and its chain is under
// its share. When the queue is full it stops: the rest are still in the
// database, and the next tick starts again from the newest.
func (e *Enricher) feed(ctx context.Context, queue chan<- chain.Pool, sched *schedule) error {
	t := time.NewTicker(time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			pools, err := e.store.PoolsNeedingEnrichment(ctx, 200)
			if err != nil {
				e.log.Debug("could not list pools needing enrichment", "error", err)
				continue
			}
			obs.EnrichmentQueueDepth.WithLabelValues("pending").Set(float64(len(pools)))
		offer:
			for _, p := range pools {
				if !sched.claim(p) {
					continue
				}
				select {
				case queue <- p:
				default:
					sched.release(p)
					break offer
				}
			}
		}
	}
}

// enrich runs the stages for one pool. Each stage is independent: a failure in
// one leaves its fields null and the others still land.
func (e *Enricher) enrich(ctx context.Context, p chain.Pool) error {
	ch, ok := e.reg.Get(p.ChainID)
	if !ok || ch.Kind != chain.KindEVM {
		return nil
	}
	evm := e.pool.EVM(p.ChainID)

	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	// Enrichment yields to discovery and market tracking on every endpoint.
	ctx = rpcpool.Background(ctx)

	// Stage 1: token identities. Everything else reads better with them, and
	// the pair name in the feed depends on them.
	t0, t1 := e.resolveTokens(ctx, evm, ch, p)
	if t0 != nil {
		p.Token0 = t0
	}
	if t1 != nil {
		p.Token1 = t1
	}

	market := store.Market{}
	if p.Token0 != nil && p.Token1 != nil && p.Token0.Symbol != "" && p.Token1.Symbol != "" {
		market.Pair = chain.Orient(ch, p.Token0, p.Token1).Name()
		p.Pair = market.Pair
	}

	// Liquidity, volume, traders and price change are not computed here. The
	// market tracker in package ingest fills them for each new pair while it is
	// young, from one chain-wide log query per tick; recomputing them per pool on
	// this schedule cost a set of calls per pool forever and, on free rate
	// budgets, left the columns empty. It would also race the tracker's figures.

	if err := e.store.UpdateMarket(ctx, p.ChainID, p.Address, market); err != nil {
		return err
	}

	// Stage 4: safety probes and the score.
	inputs := e.riskInputs(ctx, evm, ch, p)
	result := e.scorer.Score(inputs)
	if err := e.store.UpdateRisk(ctx, p.ChainID, p.Address, result.Summary,
		result.Inputs, e.scorer.Weights()); err != nil {
		return err
	}
	p.Risk = &result.Summary

	e.sink.PoolEnriched(ctx, p)
	return nil
}

// resolveTokens reads both tokens' metadata in a single multicall where the
// chain has Multicall3, which almost all do. One round trip instead of eight
// matters when the rate budget is a free tier.
func (e *Enricher) resolveTokens(ctx context.Context, evm *rpcpool.EVM, ch chain.Chain, p chain.Pool) (*chain.Token, *chain.Token) {
	if p.Token0 == nil || p.Token1 == nil {
		return nil, nil
	}
	addrs := []string{p.Token0.Address, p.Token1.Address}

	// Skip tokens already resolved: metadata is immutable in practice and
	// re-reading it on every pass would be pure waste.
	var out [2]*chain.Token
	var need []int
	for i, a := range addrs {
		if rec, err := e.store.GetToken(ctx, p.ChainID, a); err == nil && rec.Symbol != "" && rec.Decimals != nil {
			out[i] = &chain.Token{Address: a, Symbol: rec.Symbol, Name: rec.Name, Decimals: *rec.Decimals}
			if rec.TotalSupply != nil {
				out[i].TotalSupply = rec.TotalSupply.String()
			}
			continue
		}
		if chain.IsNative(a) {
			// Native ETH/BNB in a v4 pool is the zero address. There is no
			// contract to ask for a symbol — a call to it "succeeds" with empty
			// data — so its identity comes from the chain descriptor instead.
			dec := 18
			native := &chain.Token{Address: a, Symbol: ch.NativeSymbol, Name: ch.NativeSymbol, Decimals: dec}
			now := time.Now().UTC()
			if err := e.store.UpsertToken(ctx, store.TokenRecord{
				ChainID: p.ChainID, Address: a, Symbol: ch.NativeSymbol, Name: ch.NativeSymbol,
				Decimals: &dec, ResolvedAt: &now,
			}); err != nil {
				e.log.Debug("could not persist native token", "chain", p.ChainID, "error", err)
			}
			out[i] = native
			continue
		}
		need = append(need, i)
	}
	if len(need) == 0 {
		return out[0], out[1]
	}

	type slot struct {
		idx    int
		method string
	}
	var calls []call3
	var slots []slot
	for _, i := range need {
		target := common.HexToAddress(addrs[i])
		for _, m := range []string{"symbol", "name", "decimals", "totalSupply"} {
			data, err := erc20.Pack(m)
			if err != nil {
				continue
			}
			calls = append(calls, call3{Target: target, AllowFailure: true, CallData: data})
			slots = append(slots, slot{idx: i, method: m})
		}
	}

	results, err := e.multicall(ctx, evm, ch, calls)
	if err != nil {
		e.log.Debug("token metadata multicall failed", "pool", p.Key(), "error", err)
		return out[0], out[1]
	}

	tokens := map[int]*chain.Token{}
	for _, i := range need {
		tokens[i] = &chain.Token{Address: addrs[i]}
	}
	for i, r := range results {
		if i >= len(slots) || !r.Success {
			continue
		}
		s := slots[i]
		tok := tokens[s.idx]
		switch s.method {
		case "symbol":
			if v, ok := decodeString("symbol", r.ReturnData); ok {
				tok.Symbol = v
			}
		case "name":
			if v, ok := decodeString("name", r.ReturnData); ok {
				tok.Name = v
			}
		case "decimals":
			if v, ok := decodeUint8("decimals", r.ReturnData); ok {
				tok.Decimals = v
			}
		case "totalSupply":
			if v, ok := decodeBigInt(erc20, "totalSupply", r.ReturnData); ok {
				tok.TotalSupply = v.String()
			}
		}
	}

	for i, tok := range tokens {
		if tok.Symbol == "" {
			// Never invent a symbol. An unnamed token shows as "?" in the pair
			// and the user can still see the address.
			continue
		}
		dec := tok.Decimals
		rec := store.TokenRecord{
			ChainID: p.ChainID, Address: tok.Address,
			Symbol: tok.Symbol, Name: tok.Name, Decimals: &dec,
		}
		if tok.TotalSupply != "" {
			if n, ok := new(big.Int).SetString(tok.TotalSupply, 10); ok {
				rec.TotalSupply = n
			}
		}
		now := time.Now().UTC()
		rec.ResolvedAt = &now
		if err := e.store.UpsertToken(ctx, rec); err != nil {
			e.log.Debug("could not persist token", "token", tok.Address, "error", err)
		}
		out[i] = tok
	}
	return out[0], out[1]
}

func (e *Enricher) multicall(ctx context.Context, evm *rpcpool.EVM, ch chain.Chain, calls []call3) ([]result3, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	if ch.Multicall3 == "" {
		return nil, fmt.Errorf("enrich: chain %s has no multicall3 configured", ch.ID)
	}
	data, err := packMulticall(calls)
	if err != nil {
		return nil, err
	}
	out, err := evm.CallContract(ctx, common.HexToAddress(ch.Multicall3), data, nil, nil)
	if err != nil {
		return nil, err
	}
	return unpackMulticall(out)
}

// refreshPrices reads each chain's reference pool to price the native token.
// This keeps the service free of any external price feed: everything shown is
// derived from the chains we already read.
func (e *Enricher) refreshPrices(ctx context.Context) error {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	e.refreshPricesOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			e.refreshPricesOnce(ctx)
		}
	}
}

func (e *Enricher) refreshPricesOnce(ctx context.Context) {
	for _, ch := range e.reg.All() {
		if ch.Kind != chain.KindEVM || ch.NativePriceRef == "" {
			continue
		}
		price, err := e.readNativePrice(ctx, ch)
		if err != nil {
			e.log.Debug("native price refresh failed", "chain", ch.ID, "error", err)
			continue
		}
		e.priceMu.Lock()
		e.native[ch.ID] = price
		e.priceMu.Unlock()

		// Persist it so the API tier can price gas without duplicating this
		// logic or reaching for an external feed. The read side treats anything
		// older than ten minutes as unavailable.
		if err := e.store.SetNativePrice(ctx, ch.ID, price); err != nil {
			e.log.Debug("could not persist native price", "chain", ch.ID, "error", err)
		}
	}
}

func (e *Enricher) readNativePrice(ctx context.Context, ch chain.Chain) (float64, error) {
	evm := e.pool.EVM(ch.ID)
	ref := common.HexToAddress(ch.NativePriceRef)

	data, err := pairV2.Pack("getReserves")
	if err != nil {
		return 0, err
	}
	raw, err := evm.CallContract(ctx, ref, data, nil, nil)
	if err != nil {
		return 0, err
	}
	r0, r1, ok := decodeReserves(raw)
	if !ok {
		return 0, fmt.Errorf("enrich: reference pool %s did not return reserves", ch.NativePriceRef)
	}

	t0Data, _ := pairV2.Pack("token0")
	t0Raw, err := evm.CallContract(ctx, ref, t0Data, nil, nil)
	if err != nil {
		return 0, err
	}
	vals, err := pairV2.Unpack("token0", t0Raw)
	if err != nil || len(vals) == 0 {
		return 0, fmt.Errorf("enrich: reference pool token0 undecodable")
	}
	token0, _ := vals[0].(common.Address)

	nativeIsToken0 := strings.EqualFold(token0.Hex(), ch.NativePriceRefNative)
	nativeRes, stableRes := r1, r0
	nativeDec, stableDec := ch.NativePriceRefNativeDecimals, ch.NativePriceRefStableDecimals
	if nativeIsToken0 {
		nativeRes, stableRes = r0, r1
	}
	if nativeDec == 0 {
		nativeDec = 18
	}
	if stableDec == 0 {
		stableDec = 6
	}

	n := scaleDown(nativeRes, nativeDec)
	s := scaleDown(stableRes, stableDec)
	if n <= 0 {
		return 0, fmt.Errorf("enrich: reference pool has no native liquidity")
	}
	return s / n, nil
}

func containsFold(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

// scaleDown converts a base-unit amount to a float with the token's decimals.
// Floats are fine here because the result is only ever displayed; every amount
// that reaches a transaction stays a big.Int end to end.
func scaleDown(v *big.Int, decimals int) float64 {
	if v == nil {
		return 0
	}
	f := new(big.Float).SetInt(v)
	div := new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
	f.Quo(f, div)
	out, _ := f.Float64()
	return out
}
