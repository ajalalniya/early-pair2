package ingest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/config"
	"github.com/earlypair/backend/internal/rpcpool"
	"github.com/earlypair/backend/internal/store"
)

func testChain() chain.Chain {
	return chain.Chain{
		ID: "test", Name: "Test", Tag: "TST", Kind: chain.KindEVM,
		BlockTime:     time.Second,
		Confirmations: 3,
		MaxLogRange:   500, // deliberately more than the provider will serve
		Enabled:       true,
		Factories: []chain.Factory{{
			Address: factoryAddr, Dex: "Uniswap v2",
			Model: chain.ModelV2, Event: chain.EventPairCreatedV2, FeeBps: 30,
		}},
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// harness wires an ingester to the fake chain behind a hostile provider.
type harness struct {
	fc    *fakeChain
	h     *hostile
	st    *memStore
	sink  *recordingSink
	ing   *Ingester
	pool  *rpcpool.Pool
	chain chain.Chain
}

func newHarness(t *testing.T, fc *fakeChain, h *hostile) *harness {
	t.Helper()
	return newMultiHarness(t, fc, h)
}

// newMultiHarness puts several providers in front of the same chain, one per
// profile. The first profile gets the highest selection weight, so it is the
// endpoint the pool prefers — the position a misbehaving provider occupies in
// the live config (publicnode is weighted above the official RPCs).
func newMultiHarness(t *testing.T, fc *fakeChain, profiles ...*hostile) *harness {
	t.Helper()

	ch := testChain()
	reg := chain.NewRegistry([]chain.Chain{ch})

	policy := config.Default().RPC
	policy.RequestTimeout = 5 * time.Second
	policy.HedgeAfter = 50 * time.Millisecond
	// The breaker must not latch during a test that deliberately fails a share
	// of requests; it is exercised directly in its own test instead.
	policy.BreakerFailures = 1000

	var endpoints []config.Endpoint
	for i, h := range profiles {
		srv := newFixtureServer(fc, h)
		t.Cleanup(srv.Close)
		endpoints = append(endpoints, config.Endpoint{
			ID: fmt.Sprintf("fixture-%d", i), ChainID: ch.ID, Transport: chain.HTTP,
			URL: srv.URL, RateLimit: 1000, Burst: 1000,
			Weight: 100 * (len(profiles) - i),
		})
	}

	pool, err := rpcpool.New(policy, endpoints, reg, quietLogger())
	if err != nil {
		t.Fatalf("rpcpool: %v", err)
	}
	h := profiles[0]

	ingCfg := config.Default().Ingest
	ingCfg.MaxBackfillBlocks = 1_000_000

	st := newMemStore()
	sink := &recordingSink{}
	ing := New(ch, ingCfg, pool, st, sink, quietLogger())

	return &harness{fc: fc, h: h, st: st, sink: sink, ing: ing, pool: pool, chain: ch}
}

// drain advances the ingester until the cursor reaches head, or the attempt
// budget runs out.
func (hs *harness) drain(t *testing.T, maxPasses int) {
	t.Helper()
	ctx := context.Background()

	for i := 0; i < maxPasses; i++ {
		// Refresh the fleet head the way the poll loop does.
		if _, err := hs.pool.EVM(hs.chain.ID).BlockNumber(ctx); err != nil {
			continue
		}
		if err := hs.ing.advance(ctx); err != nil {
			// A hostile provider makes individual passes fail; that is the
			// point. The loop retries, exactly as the poll loop would.
			continue
		}
		cur, ok, _ := hs.st.GetCursor(ctx, hs.chain.ID)
		if ok && cur.LastBlock >= hs.fc.head() {
			return
		}
	}
	cur, _, _ := hs.st.GetCursor(ctx, hs.chain.ID)
	t.Fatalf("cursor stuck at %d after %d passes (head %d)", cur.LastBlock, maxPasses, hs.fc.head())
}

func (hs *harness) start(t *testing.T, from uint64) {
	t.Helper()
	ctx := context.Background()
	if err := hs.st.SetCursor(ctx, hs.chain.ID, store.Cursor{LastBlock: from}); err != nil {
		t.Fatal(err)
	}
}

// TestIngestUnderHostileProvider is the central claim of the poll-is-truth
// design: with a provider that throttles, refuses ranges and returns errors,
// every pool is still found, and found exactly once.
func TestIngestUnderHostileProvider(t *testing.T) {
	fc := newFakeChain(0)

	// 60 blocks; a pool every seventh, two in one block to exercise log index.
	var expected []string
	for i := 1; i <= 60; i++ {
		var pools []string
		if i%7 == 0 {
			pools = append(pools, poolAddr(i, 0))
		}
		if i == 21 {
			pools = append(pools, poolAddr(i, 1))
		}
		fc.append(pools)
		expected = append(expected, pools...)
	}

	h := &hostile{
		rateLimitEvery:   4,  // one request in four is throttled
		maxLogRange:      11, // provider refuses anything wider, despite our 500
		serverErrorEvery: 9,
	}
	hs := newHarness(t, fc, h)
	hs.start(t, 0)
	hs.drain(t, 400)

	got := hs.sink.detectedAddresses()

	for _, addr := range expected {
		key := strings.ToLower(addr)
		switch got[key] {
		case 0:
			t.Errorf("pool %s was never detected", addr)
		case 1:
		default:
			t.Errorf("pool %s was detected %d times; detection must be exactly once", addr, got[key])
		}
	}
	if len(got) != len(expected) {
		t.Errorf("detected %d distinct pools, expected %d", len(got), len(expected))
	}
	if n := hs.st.insertCount(); n != len(expected) {
		t.Errorf("store took %d inserts for %d pools; the dedupe index is not holding", n, len(expected))
	}

	_, limited, refused, errored, getLogs := h.stats()
	if limited == 0 {
		t.Error("the provider never throttled; the test is not exercising rate limiting")
	}
	if refused == 0 {
		t.Error("the provider never refused a range; the test is not exercising bisection")
	}
	if errored == 0 {
		t.Error("the provider never failed; the test is not exercising retries")
	}
	t.Logf("survived %d throttles, %d range refusals, %d server errors across %d getLogs calls",
		limited, refused, errored, getLogs)
}

// TestReplayIsIdempotent rewinds the cursor and re-reads a range that has
// already been processed — what happens after a crash, a redeploy, or a reorg
// rescan. Nothing may be emitted twice.
func TestReplayIsIdempotent(t *testing.T) {
	fc := newFakeChain(0)
	var expected []string
	for i := 1; i <= 30; i++ {
		var pools []string
		if i%5 == 0 {
			pools = append(pools, poolAddr(i, 0))
		}
		fc.append(pools)
		expected = append(expected, pools...)
	}

	hs := newHarness(t, fc, &hostile{maxLogRange: 8})
	hs.start(t, 0)
	hs.drain(t, 200)

	firstPass := len(hs.sink.detected)
	if firstPass != len(expected) {
		t.Fatalf("first pass detected %d pools, expected %d", firstPass, len(expected))
	}

	// Rewind as if the process had died halfway and resumed from an older
	// cursor, then re-read the whole range.
	hs.start(t, 0)
	hs.drain(t, 200)

	if got := len(hs.sink.detected); got != firstPass {
		t.Errorf("replaying the same range emitted %d pools in total, expected %d; "+
			"re-reading a processed range must be a no-op", got, firstPass)
	}
	if n := hs.st.insertCount(); n != len(expected) {
		t.Errorf("store took %d inserts across two passes over %d pools", n, len(expected))
	}
}

// TestReorgRewindsAndRescans covers the case the cursor design exists for: the
// chain changes underneath a processed range.
func TestReorgRewindsAndRescans(t *testing.T) {
	fc := newFakeChain(0)
	for i := 1; i <= 20; i++ {
		var pools []string
		if i == 15 {
			pools = append(pools, "0x00000000000000000000000000000000000000f0")
		}
		fc.append(pools)
	}

	hs := newHarness(t, fc, &hostile{maxLogRange: 10})
	hs.start(t, 0)
	hs.drain(t, 200)

	orphan := strings.ToLower("0x00000000000000000000000000000000000000f0")
	if hs.sink.detectedAddresses()[orphan] != 1 {
		t.Fatalf("the pre-reorg pool was not detected once: %v", hs.sink.detectedAddresses())
	}

	// Blocks 15 onward are replaced. The old pool never happened; a different
	// one did, plus a pool that survives on both chains at a deeper block.
	survivor := "0x00000000000000000000000000000000000000f1"
	fc.reorg(15, map[uint64][]string{
		15: {survivor},
		16: nil, 17: nil, 18: nil, 19: nil, 20: nil,
		21: nil, 22: nil,
	})

	hs.drain(t, 200)

	detected := hs.sink.detectedAddresses()
	if detected[strings.ToLower(survivor)] != 1 {
		t.Errorf("the post-reorg pool was not detected exactly once: %v", detected)
	}

	// The orphaned pool must be marked reorged, not left tradeable.
	for _, p := range hs.st.livePools() {
		if strings.EqualFold(p.Address, orphan) {
			t.Errorf("orphaned pool %s is still live after the reorg (state %q)", orphan, p.State)
		}
	}

	var sawReorgState bool
	hs.sink.mu.Lock()
	for _, p := range hs.sink.states {
		if strings.EqualFold(p.Address, orphan) && p.State == chain.StateReorged {
			sawReorgState = true
		}
	}
	hs.sink.mu.Unlock()
	if !sawReorgState {
		t.Error("clients were never told the orphaned pool was reorged")
	}
}

// TestCursorSurvivesTotalProviderFailure checks the property that makes polling
// the source of truth: when every request fails, the cursor does not move and
// no pool is lost — the work is simply deferred.
func TestCursorSurvivesTotalProviderFailure(t *testing.T) {
	fc := newFakeChain(0)
	for i := 1; i <= 20; i++ {
		var pools []string
		if i == 10 {
			pools = append(pools, poolAddr(i, 0))
		}
		fc.append(pools)
	}

	// Every request fails.
	h := &hostile{serverErrorEvery: 1}
	hs := newHarness(t, fc, h)
	hs.start(t, 0)

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_ = hs.ing.advance(ctx)
	}

	cur, _, _ := hs.st.GetCursor(ctx, hs.chain.ID)
	if cur.LastBlock != 0 {
		t.Errorf("cursor advanced to %d while every request was failing; "+
			"it must not move past work that was not done", cur.LastBlock)
	}
	if len(hs.sink.detected) != 0 {
		t.Errorf("pools were emitted from a provider that answered nothing")
	}

	// Recovery: the provider comes back and the deferred work completes.
	h.mu.Lock()
	h.serverErrorEvery = 0
	h.mu.Unlock()

	hs.drain(t, 200)
	if got := hs.sink.detectedAddresses()[strings.ToLower(poolAddr(10, 0))]; got != 1 {
		t.Errorf("the pool was detected %d times after recovery, expected once", got)
	}
}

func poolAddr(block, index int) string {
	return fmt.Sprintf("0x%040x", block*1000+index)
}

// TestAddresslessRefusalDoesNotStallDetection reproduces what the live indexer
// hit on BSC and Arbitrum: the preferred endpoint refuses any getLogs query
// without an address filter. Pool detection must be unaffected, and watched
// topics must still be served by an endpoint that permits the query.
func TestAddresslessRefusalDoesNotStallDetection(t *testing.T) {
	fc := newFakeChain(0)
	var expected []string
	for i := 1; i <= 30; i++ {
		var pools []string
		if i%6 == 0 {
			pools = append(pools, poolAddr(i, 0))
		}
		fc.append(pools)
		expected = append(expected, pools...)
	}

	strict := &hostile{requireAddress: true} // preferred, like publicnode
	lenient := &hostile{}
	hs := newMultiHarness(t, fc, strict, lenient)

	// A user watching PairCreated: the watch query carries no address filter.
	hs.st.watched = []string{chain.TopicPairCreatedV2.Hex()}
	hs.ing.refreshWatchTopics(context.Background())

	hs.start(t, 0)
	hs.drain(t, 100)

	got := hs.sink.detectedAddresses()
	for _, addr := range expected {
		if n := got[strings.ToLower(addr)]; n != 1 {
			t.Errorf("pool %s detected %d times, expected exactly once", addr, n)
		}
	}
	if hs.st.matchCount() != len(expected) {
		t.Errorf("recorded %d topic matches, expected %d; the watch query should have "+
			"been routed to the endpoint that permits it", hs.st.matchCount(), len(expected))
	}

	ep, _ := hs.pool.ByID("fixture-0")
	if !ep.Refuses(rpcpool.CapLogsNoAddress, 0) {
		t.Error("the strict endpoint was not remembered as refusing address-less queries")
	}
	if ep.Refuses(rpcpool.CapLogs, 0) {
		t.Error("the strict endpoint was wrongly barred from addressed log queries it serves")
	}

	// Once remembered, it must not be asked again: re-asking on every tick is
	// exactly the log spam the live run produced.
	strict.mu.Lock()
	before := strict.addressRefusals
	strict.mu.Unlock()
	fc.append([]string{poolAddr(99, 0)})
	hs.drain(t, 20)
	strict.mu.Lock()
	after := strict.addressRefusals
	strict.mu.Unlock()
	if after != before {
		t.Errorf("the strict endpoint was asked %d more address-less queries after being marked", after-before)
	}
}

// TestLogRefusingEndpointIsRoutedAround reproduces the other live failure: an
// endpoint that refuses every log query while answering eth_blockNumber and
// headers perfectly, so its health score never drops and the pool keeps
// preferring it. The healthy endpoint behind it must carry the chain.
func TestLogRefusingEndpointIsRoutedAround(t *testing.T) {
	fc := newFakeChain(0)
	var expected []string
	for i := 1; i <= 25; i++ {
		var pools []string
		if i%5 == 0 {
			pools = append(pools, poolAddr(i, 0))
		}
		fc.append(pools)
		expected = append(expected, pools...)
	}

	gated := &hostile{refuseLogs: true}
	healthy := &hostile{}
	hs := newMultiHarness(t, fc, gated, healthy)
	hs.start(t, 0)
	hs.drain(t, 50)

	got := hs.sink.detectedAddresses()
	for _, addr := range expected {
		if n := got[strings.ToLower(addr)]; n != 1 {
			t.Errorf("pool %s detected %d times, expected exactly once", addr, n)
		}
	}

	// A plain log refusal is only trusted after three strikes (load-balanced
	// endpoints refuse intermittently), each sweep costing a request and one
	// immediate retry. Keep producing blocks: within three strikes the endpoint
	// must be routed around, and from then on never asked again.
	asked := func() int {
		gated.mu.Lock()
		defer gated.mu.Unlock()
		return gated.logsRefusals
	}
	ep, _ := hs.pool.ByID("fixture-0")
	barredAt := -1
	for round := 0; round < 6; round++ {
		if barredAt < 0 && ep.Refuses(rpcpool.CapLogs, 0) {
			barredAt = round
		}
		before := asked()
		fc.append([]string{poolAddr(90+round, 0)})
		hs.drain(t, 20)
		if barredAt >= 0 && asked() != before {
			t.Errorf("round %d: the endpoint was asked %d more times after being routed around",
				round, asked()-before)
		}
	}
	if barredAt < 0 || barredAt > 3 {
		t.Errorf("the endpoint refusing every log query was not routed around within three strikes (barred at round %d)", barredAt)
	}
	if asked() > 6 {
		t.Errorf("the refusing endpoint was asked %d times in total; three strikes cost at most six", asked())
	}
	if got := len(hs.sink.detectedAddresses()); got != len(expected)+6 {
		t.Errorf("detected %d pools, expected %d; the healthy endpoint must carry every range", got, len(expected)+6)
	}
}

// TestWatchSweepNeverBlocksCreation: when no endpoint will serve an address-less
// query, watched topics miss the range — but pools are still detected and the
// cursor still moves. A secondary feature must not hold the product hostage.
func TestWatchSweepNeverBlocksCreation(t *testing.T) {
	fc := newFakeChain(0)
	var expected []string
	for i := 1; i <= 20; i++ {
		var pools []string
		if i%4 == 0 {
			pools = append(pools, poolAddr(i, 0))
		}
		fc.append(pools)
		expected = append(expected, pools...)
	}

	hs := newMultiHarness(t, fc, &hostile{requireAddress: true}, &hostile{requireAddress: true})
	hs.st.watched = []string{chain.TopicPairCreatedV2.Hex()}
	hs.ing.refreshWatchTopics(context.Background())

	hs.start(t, 0)
	hs.drain(t, 50)

	if got := len(hs.sink.detectedAddresses()); got != len(expected) {
		t.Errorf("detected %d pools, expected %d; watch refusals must not affect detection",
			got, len(expected))
	}
	// The watched topic here is PairCreated, which the registered factories'
	// own creation logs also emit. Those arrive through the addressed query, so
	// they are still recorded as matches — correct, partial data. What is lost
	// is only emissions from contracts outside the factory list, which nothing
	// in this fake chain produces.
	if got := hs.st.matchCount(); got != len(expected) {
		t.Errorf("recorded %d topic matches, expected the %d creation logs seen through "+
			"the addressed query", got, len(expected))
	}
}

// TestStartBeginsAtHead: a start does not sweep the blocks missed while the
// indexer was stopped; it begins at the head. With resumeOnStart it resumes
// from the stored cursor instead.
func TestStartBeginsAtHead(t *testing.T) {
	fc := newFakeChain(0)
	for i := 1; i <= 40; i++ {
		fc.append(nil)
	}
	ctx := context.Background()

	hs := newHarness(t, fc, &hostile{})
	hs.start(t, 10) // stopped at block 10; the chain has moved on to 40
	cur, err := hs.ing.resolveStart(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cur.LastBlock != fc.head() {
		t.Errorf("start cursor = %d, want the head %d", cur.LastBlock, fc.head())
	}
	if stored, _, _ := hs.st.GetCursor(ctx, hs.chain.ID); stored.LastBlock != fc.head() {
		t.Errorf("stored cursor = %d, want the head %d", stored.LastBlock, fc.head())
	}

	resume := newHarness(t, fc, &hostile{})
	resume.ing.cfg.ResumeOnStart = true
	resume.start(t, 10)
	if cur, err := resume.ing.resolveStart(ctx); err != nil || cur.LastBlock != 10 {
		t.Errorf("with resumeOnStart: cursor = %d (err %v), want 10", cur.LastBlock, err)
	}
}

// TestCatchUpPoolsAreNotAnnouncedAsNew covers what a restart after downtime
// does: the indexer sweeps hours of history. Those pools are real and must be
// stored, but only pools from the live window may reach clients as new, or the
// feed is buried under a flood of old pairs the moment the indexer comes back.
func TestCatchUpPoolsAreNotAnnouncedAsNew(t *testing.T) {
	fc := newFakeChain(0)
	// Block 10 lands 306s ago (outside the 5-minute window); block 11, 294s ago.
	now := uint64(time.Now().Unix())
	fc.baseTime = now - 300 - 10*12 - 6

	old, fresh := poolAddr(5, 0), poolAddr(15, 0)
	for i := 1; i <= 20; i++ {
		switch i {
		case 5:
			fc.append([]string{old})
		case 15:
			fc.append([]string{fresh})
		default:
			fc.append(nil)
		}
	}

	hs := newHarness(t, fc, &hostile{})
	hs.start(t, 0)
	hs.drain(t, 50)

	byAddr := map[string]bool{}
	hs.sink.mu.Lock()
	for _, p := range hs.sink.detected {
		byAddr[strings.ToLower(p.Address)] = p.Backfilled
	}
	hs.sink.mu.Unlock()

	if backfilled, ok := byAddr[strings.ToLower(old)]; !ok || !backfilled {
		t.Errorf("a pool created ~6 minutes ago was not marked backfilled (found=%v)", ok)
	}
	if backfilled, ok := byAddr[strings.ToLower(fresh)]; !ok || backfilled {
		t.Errorf("a pool created within the live window was marked backfilled (found=%v)", ok)
	}
}

// TestDepthLimitedEndpointStillServesTheTip reproduces drpc's free tier: log
// queries far behind head are refused, queries near it are served. During a
// catch-up the deep ranges go elsewhere — and once caught up, the endpoint must
// still be usable at the tip rather than barred for everything.
func TestDepthLimitedEndpointStillServesTheTip(t *testing.T) {
	fc := newFakeChain(0)
	var expected []string
	for i := 1; i <= 60; i++ {
		var pools []string
		if i%10 == 0 {
			pools = append(pools, poolAddr(i, 0))
		}
		fc.append(pools)
		expected = append(expected, pools...)
	}

	shallow := &hostile{archiveDepth: 15, maxLogRange: 10} // preferred, like drpc
	deep := &hostile{maxLogRange: 10}
	hs := newMultiHarness(t, fc, shallow, deep)
	hs.start(t, 0)
	hs.drain(t, 100)

	if got := len(hs.sink.detectedAddresses()); got != len(expected) {
		t.Fatalf("detected %d pools during the catch-up, expected %d", got, len(expected))
	}

	ep, _ := hs.pool.ByID("fixture-0")
	if ep.Refuses(rpcpool.CapLogs, 0) {
		t.Error("an endpoint that only refuses deep ranges was barred at the tip")
	}

	// A new pool at the tip: the shallow endpoint must be the one serving it.
	shallow.mu.Lock()
	before := shallow.getLogsCalls
	shallow.mu.Unlock()
	fc.append([]string{poolAddr(61, 0)})
	hs.drain(t, 20)
	shallow.mu.Lock()
	after := shallow.getLogsCalls
	shallow.mu.Unlock()
	if after == before {
		t.Error("the preferred endpoint was not used for a tip query after the catch-up")
	}
}

// TestIntermittentArchiveRefusalDoesNotStall reproduces publicnode: the same
// log query is refused by some backends and served by others. With it as the
// only endpoint, ingestion must still reach the head.
func TestIntermittentArchiveRefusalDoesNotStall(t *testing.T) {
	fc := newFakeChain(0)
	var expected []string
	for i := 1; i <= 40; i++ {
		var pools []string
		if i%8 == 0 {
			pools = append(pools, poolAddr(i, 0))
		}
		fc.append(pools)
		expected = append(expected, pools...)
	}

	flaky := &hostile{refuseLogsEvery: 2, maxLogRange: 10}
	hs := newHarness(t, fc, flaky)
	hs.start(t, 0)
	hs.drain(t, 200)

	if got := len(hs.sink.detectedAddresses()); got != len(expected) {
		t.Errorf("detected %d pools behind an intermittently refusing endpoint, expected %d", got, len(expected))
	}
}
