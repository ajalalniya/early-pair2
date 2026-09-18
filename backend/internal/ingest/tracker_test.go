package ingest

import (
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/config"
)

const (
	usdcAddr  = "0x833589fcd6edb6e08f4c7c32d4f71b54bda02913"
	tokenAddr = "0x0000000000000000000000000000000000000001" // sorts below USDC: token0
	wethBase  = "0x4200000000000000000000000000000000000006"
)

func trackerChain() chain.Chain {
	return chain.Chain{ID: "base", WrappedNative: wethBase, Stables: []string{usdcAddr}}
}

func newTestTracker() *tracker {
	return &tracker{i: &Ingester{ch: trackerChain()}, pools: map[string]*trackedPool{}}
}

func wei(n int64, decimals int) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
}

func word(v *big.Int) []byte {
	b := make([]byte, 32)
	if v.Sign() < 0 {
		v = new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 256), v)
	}
	v.FillBytes(b)
	return b
}

func concat(words ...[]byte) []byte {
	var out []byte
	for _, w := range words {
		out = append(out, w...)
	}
	return out
}

func syncLog(block uint64, index uint, r0, r1 *big.Int) types.Log {
	return types.Log{
		Topics: []common.Hash{chain.TopicSyncV2}, Data: concat(word(r0), word(r1)),
		BlockNumber: block, Index: index, TxHash: common.BigToHash(big.NewInt(int64(block*1000 + uint64(index)))),
	}
}

// v2 Swap: amount0In, amount1In, amount0Out, amount1Out.
func swapV2Log(block uint64, index uint, a0In, a1In, a0Out, a1Out *big.Int) types.Log {
	return types.Log{
		Topics:      []common.Hash{chain.TopicSwapV2, {}, {}},
		Data:        concat(word(a0In), word(a1In), word(a0Out), word(a1Out)),
		BlockNumber: block, Index: index, TxHash: common.BigToHash(big.NewInt(int64(block*1000 + uint64(index)))),
	}
}

func newV2Pool() *trackedPool {
	t0 := &chain.Token{Address: tokenAddr, Symbol: "NEW"}
	t1 := &chain.Token{Address: usdcAddr, Symbol: "USDC"}
	p := chain.Pool{Address: "0xpool", Model: chain.ModelV2, Token0: t0, Token1: t1}
	return newTrackedPool(p, chain.Orient(trackerChain(), t0, t1), 18, 6)
}

func TestTrackerV2Metrics(t *testing.T) {
	tr := newTestTracker()
	tp := newV2Pool()

	// Launch: 1,000,000 NEW against 10,000 USDC → price 0.01.
	tp.apply(syncLog(100, 0, wei(1_000_000, 18), wei(10_000, 6)))
	// A buy: 1,000 USDC in, some NEW out. Then reserves move to price 0.0121.
	tp.apply(swapV2Log(101, 0, big.NewInt(0), wei(1_000, 6), wei(90_909, 18), big.NewInt(0)))
	tp.apply(syncLog(101, 1, wei(909_091, 18), wei(11_000, 6)))
	// A sell from a different wallet: 5,000 NEW in, ~60 USDC out.
	tp.apply(swapV2Log(102, 0, wei(5_000, 18), big.NewInt(0), big.NewInt(0), wei(60, 6)))

	tp.swaps[0].signer = "0xbuyer"
	tp.swaps[1].signer = "0xseller"

	m := tr.metrics(tp)
	if *m.Buyers1h != 1 || *m.Sellers1h != 1 {
		t.Errorf("buyers/sellers = %d/%d, want 1/1", *m.Buyers1h, *m.Sellers1h)
	}
	if m.Volume1hUSD == nil || math.Abs(*m.Volume1hUSD-1060) > 1e-6 {
		t.Errorf("volume = %v, want 1060 (1,000 in + 60 out, USDC-quoted)", m.Volume1hUSD)
	}
	wantChange := ((11_000.0/909_091)/(10_000.0/1_000_000) - 1) * 100
	if m.PriceChange1h == nil || math.Abs(*m.PriceChange1h-wantChange) > 1e-6 {
		t.Errorf("price change = %v, want %.4f%%", m.PriceChange1h, wantChange)
	}
	// Liquidity: both sides valued at the latest price, which for a balanced
	// v2 pool is twice the quote side.
	if m.LiquidityUSD == nil || math.Abs(*m.LiquidityUSD-22_000) > 1 {
		t.Errorf("liquidity = %v, want ~22,000", m.LiquidityUSD)
	}
}

func TestTrackerNeverCountsAnEventTwice(t *testing.T) {
	tp := newV2Pool()
	swap := swapV2Log(101, 0, big.NewInt(0), wei(500, 6), wei(10, 18), big.NewInt(0))
	tp.apply(swap)
	tp.apply(swap) // a rescan delivers the same log again
	if len(tp.swaps) != 1 {
		t.Errorf("the same swap was recorded %d times", len(tp.swaps))
	}
}

// TestTrackerOutOfOrderPriceTrail: adding a pair mid-window rescans from its
// creation block, so older events can arrive after newer ones. The opening
// price must stay the earliest and the current price the latest.
func TestTrackerOutOfOrderPriceTrail(t *testing.T) {
	tp := newV2Pool()
	tp.apply(syncLog(200, 0, wei(1_000, 18), wei(2_000, 6))) // later: price 2
	tp.apply(syncLog(100, 0, wei(1_000, 18), wei(1_000, 6))) // earlier: price 1
	if open, last, _ := tp.priceChange(0); !near(open, 1) || !near(last, 2) {
		t.Errorf("open=%v last=%v, want 1 and 2", open, last)
	}
	if tp.r1.Cmp(wei(2_000, 6)) != 0 {
		t.Error("an older Sync overwrote the latest reserves")
	}
}

// TestTrackerV4NativeLiquidity: a v4 pool quoted in native ETH, with its
// opening price from Initialize and liquidity from ModifyLiquidity.
func TestTrackerV4NativeLiquidity(t *testing.T) {
	tr := newTestTracker()
	tr.nativeUSD, tr.nativeOK = 2000, true

	native := &chain.Token{Address: chain.NativeAddress, Symbol: "ETH"}
	token := &chain.Token{Address: "0x00000000000000000000000000000000000000ff", Symbol: "PEPE"}
	id := "0x" + common.Bytes2Hex(common.LeftPadBytes([]byte{0xab}, 32))
	p := chain.Pool{Address: id, Model: chain.ModelCLMM, Token0: native, Token1: token}
	tp := newTrackedPool(p, chain.Orient(trackerChain(), native, token), 18, 18)
	if !tp.v4 {
		t.Fatal("a PoolId was not recognised as a v4 pool")
	}
	if tp.o.BaseIsToken0 {
		t.Fatal("native ETH was oriented as the traded token")
	}

	// Opening price: sqrtPriceX96 = 2^96 → raw price 1 (1 PEPE per ETH).
	sqrt := new(big.Int).Lsh(big.NewInt(1), 96)
	initData := concat(word(big.NewInt(3000)), word(big.NewInt(60)), word(big.NewInt(0)), word(sqrt), word(big.NewInt(0)))
	tp.apply(types.Log{
		Topics: []common.Hash{chain.TopicInitializeV4, common.HexToHash(id), {}, {}},
		Data:   initData, BlockNumber: 10, TxHash: common.HexToHash("0x01"),
	})

	// A symmetric position around the current price.
	liq := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	mlData := concat(word(big.NewInt(-600)), word(big.NewInt(600)), word(liq), word(big.NewInt(0)))
	tp.apply(types.Log{
		Topics: []common.Hash{chain.TopicModifyLiquidity, common.HexToHash(id), {}},
		Data:   mlData, BlockNumber: 10, Index: 1, TxHash: common.HexToHash("0x02"),
	})

	m := tr.metrics(tp)
	if m.PriceChange1h == nil || math.Abs(*m.PriceChange1h) > 1e-9 {
		t.Errorf("price change with no trades = %v, want 0", m.PriceChange1h)
	}
	if m.LiquidityUSD == nil || *m.LiquidityUSD <= 0 {
		t.Fatalf("liquidity = %v, want a positive dollar value", m.LiquidityUSD)
	}
	// At price 1 a ±600-tick range holds equal raw amounts of both sides; each
	// side is worth the same in ETH, so liquidity is twice one side times $2000.
	a0, a1 := chain.PositionAmounts(1e18, -600, 600, 1)
	want := (a0/1e18 + a1/1e18) * 2000
	if math.Abs(*m.LiquidityUSD-want)/want > 1e-6 {
		t.Errorf("liquidity = %v, want %v", *m.LiquidityUSD, want)
	}
}

// TestTrackerUnpricedQuoteLeavesDollarsEmpty: when the quote asset has no known
// dollar price, volume and liquidity stay empty rather than guessed, while
// price change and traders — which need no dollar price — still fill in.
func TestTrackerUnpricedQuoteLeavesDollarsEmpty(t *testing.T) {
	tr := newTestTracker()
	a := &chain.Token{Address: "0x00000000000000000000000000000000000000a1", Symbol: "AAA"}
	b := &chain.Token{Address: "0x00000000000000000000000000000000000000b2", Symbol: "BBB"}
	p := chain.Pool{Address: "0xpool2", Model: chain.ModelV2, Token0: a, Token1: b}
	tp := newTrackedPool(p, chain.Orient(trackerChain(), a, b), 18, 18)
	tp.apply(syncLog(1, 0, wei(100, 18), wei(100, 18)))
	tp.apply(syncLog(2, 0, wei(100, 18), wei(150, 18)))

	m := tr.metrics(tp)
	if m.Volume1hUSD != nil || m.LiquidityUSD != nil {
		t.Errorf("dollar figures invented for an unpriced quote: vol=%v liq=%v", m.Volume1hUSD, m.LiquidityUSD)
	}
	if m.PriceChange1h == nil || math.Abs(*m.PriceChange1h-50) > 1e-9 {
		t.Errorf("price change = %v, want 50%%", m.PriceChange1h)
	}
}

// TestTrackerFreezesThenDrainsSigners: at the end of its window a pair stops
// taking events but keeps resolving the signers of swaps it already saw, and is
// dropped once those are counted.
func TestTrackerFreezesThenDrainsSigners(t *testing.T) {
	tr := newTestTracker()
	tr.window = 3 * time.Minute
	tp := newV2Pool()
	tp.p.CreatedAt = time.Now().Add(-4 * time.Minute)
	tr.pools["0xpool"] = tp

	tp.apply(swapV2Log(101, 0, big.NewInt(0), wei(100, 6), wei(1, 18), big.NewInt(0)))
	tr.expire(time.Now())
	if _, ok := tr.pools["0xpool"]; !ok || !tp.frozen {
		t.Fatal("a pair with unresolved swaps was dropped at the end of its window")
	}
	if tr.cursor != 0 {
		t.Error("the scan cursor was kept with no pair left in its window")
	}

	tp.swaps[0].signer = "0xbuyer"
	tr.expire(time.Now())
	if _, ok := tr.pools["0xpool"]; ok {
		t.Error("a frozen pair with every signer resolved was kept")
	}
	if m := tr.metrics(tp); *m.Buyers1h != 1 || math.Abs(*m.Volume1hUSD-100) > 1e-9 {
		t.Errorf("buyers=%d volume=%v, want 1 and 100", *m.Buyers1h, *m.Volume1hUSD)
	}
}

// screeningTracker wires a tracker to a fake store and sink, with the bar at
// $10k liquidity or $100 volume, screened three minutes after creation.
func screeningTracker(keepIf string) (*tracker, *memStore, *recordingSink) {
	st, sink := newMemStore(), &recordingSink{}
	tr := &tracker{
		i:      &Ingester{ch: trackerChain(), store: st, sink: sink},
		window: time.Hour,
		screen: config.Screen{After: 3 * time.Minute, MinLiquidityUSD: 10_000, MinVolumeUSD: 100, KeepIf: keepIf},
		pools:  map[string]*trackedPool{},
	}
	return tr, st, sink
}

func addScreened(tr *tracker, st *memStore, tp *trackedPool, age time.Duration) {
	tp.p.ChainID = "base"
	tp.p.CreatedAt = time.Now().Add(-age)
	st.byAddr["base:"+tp.p.Address] = &tp.p
	tr.pools[tp.p.Address] = tp
}

func TestScreeningKeepsAPairThatClearsTheBar(t *testing.T) {
	tr, st, sink := screeningTracker("either")
	tp := newV2Pool() // ~$22k liquidity after these syncs, no swaps
	tp.apply(syncLog(100, 0, wei(1_000_000, 18), wei(11_000, 6)))
	addScreened(tr, st, tp, 4*time.Minute)
	tr.scannedAt = time.Now()

	tr.screenPairs(t.Context(), time.Now())

	if _, ok := tr.pools["0xpool"]; !ok || !tp.screened {
		t.Fatal("a $22k pair was not kept")
	}
	if len(sink.removed) != 0 || st.byAddr["base:0xpool"].RemovedAt != nil {
		t.Error("a kept pair was marked removed")
	}
	if st.byAddr["base:0xpool"].ScreenedAt == nil {
		t.Error("the verdict was not recorded, so a restart would judge it again")
	}
}

func TestScreeningRemovesAPairBelowBothThresholds(t *testing.T) {
	tr, st, sink := screeningTracker("either")
	tp := newV2Pool() // $200 liquidity, $50 volume
	tp.apply(syncLog(100, 0, wei(1_000, 18), wei(100, 6)))
	tp.apply(swapV2Log(101, 0, big.NewInt(0), wei(50, 6), wei(1, 18), big.NewInt(0)))
	addScreened(tr, st, tp, 4*time.Minute)
	tr.scannedAt = time.Now()

	tr.screenPairs(t.Context(), time.Now())

	if _, ok := tr.pools["0xpool"]; ok {
		t.Error("a pair under both thresholds is still tracked")
	}
	if len(sink.removed) != 1 || sink.removed[0].RemovedAt == nil {
		t.Fatalf("clients were not told about the removal: %+v", sink.removed)
	}
	if st.byAddr["base:0xpool"].RemovedAt == nil {
		t.Error("the removal was not stored")
	}
}

func TestScreeningKeepIfEitherVersusBoth(t *testing.T) {
	// $200 liquidity but $5,000 of volume.
	build := func() *trackedPool {
		tp := newV2Pool()
		tp.apply(syncLog(100, 0, wei(1_000, 18), wei(100, 6)))
		tp.apply(swapV2Log(101, 0, big.NewInt(0), wei(5_000, 6), wei(1, 18), big.NewInt(0)))
		return tp
	}
	for keepIf, wantKept := range map[string]bool{"either": true, "both": false} {
		tr, st, _ := screeningTracker(keepIf)
		addScreened(tr, st, build(), 4*time.Minute)
		tr.scannedAt = time.Now()
		tr.screenPairs(t.Context(), time.Now())
		if _, kept := tr.pools["0xpool"]; kept != wantKept {
			t.Errorf("keepIf=%s: kept=%v, want %v", keepIf, kept, wantKept)
		}
	}
}

func TestScreeningWaitsForLateFiguresNotUnknowableOnes(t *testing.T) {
	tr, st, sink := screeningTracker("either")
	native := &chain.Token{Address: chain.NativeAddress, Symbol: "ETH"}
	token := &chain.Token{Address: "0x00000000000000000000000000000000000000ff", Symbol: "NEW"}
	p := chain.Pool{Address: "0xpool", Model: chain.ModelV2, Token0: native, Token1: token}
	tp := newTrackedPool(p, chain.Orient(trackerChain(), native, token), 18, 18)
	tp.apply(syncLog(100, 0, wei(100, 18), wei(1_000_000, 18))) // 100 ETH: well over the bar
	addScreened(tr, st, tp, 4*time.Minute)

	// Due, but the last scan predates the screening time: no verdict.
	tr.scannedAt = time.Now().Add(-2 * time.Minute)
	tr.screenPairs(t.Context(), time.Now())
	if tp.screened || len(sink.removed) > 0 {
		t.Fatal("judged on figures read before the screening time")
	}

	// Scanned, but ETH's price has not loaded: the figure is late, so wait.
	tr.scannedAt = time.Now()
	tr.screenPairs(t.Context(), time.Now())
	if tp.screened || len(sink.removed) > 0 {
		t.Fatal("removed a native-quoted pair before the native price was known")
	}

	tr.nativeUSD, tr.nativeOK = 2000, true
	tr.screenPairs(t.Context(), time.Now())
	if !tp.screened {
		t.Error("a 100 ETH pair was not kept once ETH was priced")
	}
}

// TestSignerLookupsAreSharedAcrossPairs: a pair with thousands of pending swaps
// must not take the whole budget from pairs with a handful.
func TestSignerLookupsAreSharedAcrossPairs(t *testing.T) {
	h := func(n int) common.Hash { return common.BigToHash(big.NewInt(int64(n))) }
	busy := make([]common.Hash, 5000)
	for n := range busy {
		busy[n] = h(n)
	}
	quiet := []common.Hash{h(90001), h(90002)}
	other := []common.Hash{h(80001)}

	got := roundRobin([][]common.Hash{busy, quiet, other}, 6, 0)
	want := []common.Hash{h(0), h(90001), h(80001), h(1), h(90002), h(2)}
	if len(got) != len(want) {
		t.Fatalf("took %d lookups, want %d", len(got), len(want))
	}
	for n := range want {
		if got[n] != want[n] {
			t.Fatalf("lookup %d = %s, want %s: the quiet pairs were not served in turn", n, got[n], want[n])
		}
	}
	// The next tick starts from the next pair.
	if next := roundRobin([][]common.Hash{busy, quiet, other}, 1, 1); next[0] != h(90001) {
		t.Errorf("rotation did not advance: first lookup %s", next[0])
	}
	// A budget larger than the work takes all of it and stops.
	if all := roundRobin([][]common.Hash{quiet, other}, 50, 0); len(all) != 3 {
		t.Errorf("took %d, want all 3", len(all))
	}
}

// TestTrackerRollingHour: a pair watched past an hour reports the last hour, not
// its whole life. Swaps older than an hour of blocks leave volume and traders,
// and price change runs from the last price at or before the hour mark.
func TestTrackerRollingHour(t *testing.T) {
	tr := newTestTracker() // 12s blocks: an hour is 300 blocks
	tp := newV2Pool()
	tp.head = 1000 // the hour is blocks 701-1000

	tp.apply(syncLog(600, 0, wei(1_000, 18), wei(1_000, 6)))                           // price 1, long ago
	tp.apply(swapV2Log(600, 1, big.NewInt(0), wei(500, 6), wei(1, 18), big.NewInt(0))) // an old $500 buy
	tp.apply(syncLog(690, 0, wei(1_000, 18), wei(2_000, 6)))                           // price 2: opens the hour
	tp.apply(swapV2Log(800, 0, wei(1, 18), big.NewInt(0), big.NewInt(0), wei(100, 6))) // a $100 sell in the hour
	tp.apply(syncLog(900, 0, wei(1_000, 18), wei(3_000, 6)))                           // price 3 now
	tp.swaps[0].signer, tp.swaps[1].signer = "0xold", "0xnew"

	m := tr.metrics(tp)
	if math.Abs(*m.Volume1hUSD-100) > 1e-9 {
		t.Errorf("volume = %v, want 100: the old swap is outside the hour", *m.Volume1hUSD)
	}
	if *m.Buyers1h != 0 || *m.Sellers1h != 1 {
		t.Errorf("buyers/sellers = %d/%d, want 0/1", *m.Buyers1h, *m.Sellers1h)
	}
	if math.Abs(*m.PriceChange1h-50) > 1e-9 {
		t.Errorf("price change = %v, want 50%% (2 at the hour mark to 3 now)", *m.PriceChange1h)
	}

	tp.prune(tp.cutoff(tr.blocksIn(time.Hour)))
	if len(tp.swaps) != 1 || len(tp.prices) != 2 {
		t.Errorf("after pruning: %d swaps and %d prices, want 1 and 2 (the hour's opening price kept)", len(tp.swaps), len(tp.prices))
	}
	if again := tr.metrics(tp); !sameMarket(m, again) {
		t.Error("pruning changed the figures")
	}
}

// TestFavoritesStayTrackedAndAreNeverRemoved: a starred pair is not frozen at
// the end of the window and not judged at the screening time; unstarred, it is
// treated like any other pair of its age.
func TestFavoritesStayTrackedAndAreNeverRemoved(t *testing.T) {
	tr, st, sink := screeningTracker("both")
	tr.window = 10 * time.Minute
	tp := newV2Pool() // $200 liquidity: under the bar
	tp.apply(syncLog(100, 0, wei(1_000, 18), wei(100, 6)))
	tp.favorite = true
	addScreened(tr, st, tp, 4*time.Hour)
	tr.scannedAt = time.Now()

	tr.screenPairs(t.Context(), time.Now())
	tr.expire(time.Now())
	if _, ok := tr.pools["0xpool"]; !ok || tp.frozen || len(sink.removed) > 0 {
		t.Fatalf("a favorite was frozen or removed (tracked=%v frozen=%v removed=%d)", ok, tp.frozen, len(sink.removed))
	}

	tp.favorite = false
	tr.screenPairs(t.Context(), time.Now())
	if len(sink.removed) != 1 {
		t.Error("an unstarred pair under the bar was not judged")
	}
}

// TestPlanHistory: a joining pair reads its own history up to the shared
// cursor, at most historyCap back; a v4 pair whose history was cut short has
// unknown liquidity rather than a figure missing its early positions.
func TestPlanHistory(t *testing.T) {
	tr := newTestTracker() // 12s blocks: 24 hours is 7,200 blocks
	tr.cursor = 100_000

	young := newV2Pool()
	young.p.CreatedBlock = 99_990
	tr.planHistory(young)
	if young.fillFrom != 99_990 || young.fillTo != 100_000 || young.partial || young.warm() {
		t.Errorf("young pair: fill %d-%d partial=%v", young.fillFrom, young.fillTo, young.partial)
	}

	unborn := newV2Pool()
	unborn.p.CreatedBlock = 100_050 // after the cursor: the shared scan covers it
	tr.planHistory(unborn)
	if !unborn.warm() {
		t.Error("a pair created past the cursor was given history to read")
	}

	id := "0x" + common.Bytes2Hex(common.LeftPadBytes([]byte{0xcd}, 32))
	old := newTrackedPool(chain.Pool{Address: id, Model: chain.ModelCLMM, CreatedBlock: 1_000},
		chain.Orientation{BaseIsToken0: true}, 18, 18)
	tr.planHistory(old)
	if old.fillFrom != 100_000-7_200 || !old.partial {
		t.Errorf("old pair: fill from %d partial=%v, want %d and partial", old.fillFrom, old.partial, 100_000-7_200)
	}
	if _, _, known := old.holdings(); known {
		t.Error("a v4 pair with truncated history reported its liquidity")
	}
}

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }

// Volume acceleration answers "is this pair speeding up?": the last minute's
// volume over the pair's own average minute. The pair's launch block bounds the
// span, not the hour — measuring a ten-minute-old pair against an hour would
// report every new pair as accelerating.
func acceleratingPool(created uint64, swaps []swapRecord, head uint64) *trackedPool {
	return &trackedPool{
		p:     chain.Pool{ChainID: "base", Address: "0xpool", CreatedBlock: created},
		swaps: swaps,
		head:  head,
	}
}

func at(block uint64) uint64 { return block << 20 }

func TestVolumeAccelerationComparesTheLastMinuteWithTheAverage(t *testing.T) {
	// 30 blocks to the minute, a pair 10 minutes old: 300 blocks of history.
	const minute, head = 30, 1300
	var quiet []swapRecord
	for b := uint64(1010); b <= 1300; b += 10 {
		quiet = append(quiet, swapRecord{at: at(b), quote: 10}) // 30 swaps, 10 a block-step
	}
	steady := acceleratingPool(1000, quiet, head)
	got, ok := steady.volumeAcceleration(minute, 0)
	if !ok {
		t.Fatal("a ten-minute-old pair with volume should have a figure")
	}
	// Trading at one rate throughout: the last minute matches the average.
	if math.Abs(got-1) > 0.05 {
		t.Errorf("steady trading = %.2f, want about 1", got)
	}

	// The same pair with a burst in its last minute.
	burst := append(append([]swapRecord{}, quiet...), swapRecord{at: at(1295), quote: 500})
	got, ok = acceleratingPool(1000, burst, head).volumeAcceleration(minute, 0)
	if !ok || got < 5 {
		t.Errorf("a burst of 500 on top of a 30-a-minute pace = %.2f, want well above 5", got)
	}

	// And one that has gone quiet: volume early, nothing lately.
	var early []swapRecord
	for b := uint64(1010); b < 1100; b += 10 {
		early = append(early, swapRecord{at: at(b), quote: 100})
	}
	got, ok = acceleratingPool(1000, early, head).volumeAcceleration(minute, 0)
	if !ok || got != 0 {
		t.Errorf("a pair that stopped trading = %.2f (ok %v), want 0", got, ok)
	}
}

func TestVolumeAccelerationStaysSilentWithoutEnoughToCompare(t *testing.T) {
	const minute = 30
	swaps := []swapRecord{{at: at(1010), quote: 10}, {at: at(1040), quote: 10}}

	// Younger than three windows: the last minute would be compared with itself.
	if _, ok := acceleratingPool(1000, swaps, 1060).volumeAcceleration(minute, 0); ok {
		t.Error("a two-minute-old pair reported an acceleration")
	}
	// Old enough, but nothing has traded.
	if _, ok := acceleratingPool(1000, nil, 1300).volumeAcceleration(minute, 0); ok {
		t.Error("a pair with no swaps reported an acceleration")
	}
	// An hour-old pair measures from the hour cutoff, not from its launch.
	long := []swapRecord{{at: at(5010), quote: 10}, {at: at(5200), quote: 10}}
	if _, ok := acceleratingPool(1000, long, 5300).volumeAcceleration(minute, 5000); !ok {
		t.Error("an older pair should still be measured, over its last hour")
	}
}
