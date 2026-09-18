package ingest

import (
	"context"
	"errors"
	"math"
	"math/big"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/config"
	"github.com/earlypair/backend/internal/obs"
	"github.com/earlypair/backend/internal/rpcpool"
	"github.com/earlypair/backend/internal/store"
)

// The market tracker fills a pair's LIQUIDITY, VOL 1H, BUYERS / SELLERS and
// PRICE 1H columns. Every new pair is watched for a short window after creation
// and screened a few minutes in: one under the feed's liquidity and volume bar
// is removed from the feed and no longer tracked. A favorite — a pair starred in
// the UI — is watched for as long as it stays starred, and is never removed.
//
// Watching a bounded set is what makes these columns affordable on public
// endpoints. Refreshing every pool the service has ever seen would cost one set
// of calls per pool per pass, forever, and on free rate budgets that meant the
// columns simply stayed empty. Instead, one chain-wide log query per tick covers
// every tracked pair, and everything is computed from those events: swaps give
// volume, traders and the price trail; Sync, Initialize and ModifyLiquidity give
// price and liquidity.
//
// The 1H columns are a rolling hour: swaps older than an hour of blocks leave
// volume and the trader counts, and price change runs from the last price at or
// before the hour mark. For a pair younger than an hour that is simply "since
// launch". A pair whose window ends keeps the figures it had then.

// Topics the tracker needs beyond the decoders in package chain. Mint and Burn
// on a v3 pool are not decoded; they only mark its token balances as stale.
var (
	topicMintV3 = chain.TopicForSignature("Mint(address,address,int24,int24,uint128,uint256,uint256)")
	topicBurnV3 = chain.TopicForSignature("Burn(address,int24,int24,uint128,uint256,uint256)")

	pairTopics = []common.Hash{
		chain.TopicSwapV2, chain.TopicSwapSolidly, chain.TopicSwapV3, chain.TopicSwapPancakeV3,
		chain.TopicSyncV2, chain.TopicSyncSolidly, chain.TopicInitializeV3,
		topicMintV3, topicBurnV3,
	}
	v4Topics = []common.Hash{chain.TopicSwapV4, chain.TopicModifyLiquidity, chain.TopicInitializeV4}
)

const (
	// historyCap bounds how far back a joining pair's own events are read: a
	// favorite starred long after launch. Past it, a v4 pair's liquidity — the
	// sum of every position since creation — is unknown rather than guessed.
	historyCap = 24 * time.Hour

	// historyQueriesPerTick bounds those reads, so starring an old pair cannot
	// crowd out the scan every tracked pair depends on.
	historyQueriesPerTick = 2

	// signerDrain is how long after its window a pair may keep resolving the
	// signers of swaps it already saw.
	signerDrain = 10 * time.Minute
)

type tracker struct {
	i      *Ingester
	window time.Duration
	every  time.Duration
	screen config.Screen

	// rotation picks which pair takes the first signer lookup each tick.
	rotation int

	// scannedAt is when a scan last reached the head with every tracked pair
	// in it. A pair is screened only on figures read after its screening time.
	scannedAt time.Time

	// signerWarnedAt rate-limits the warning that trader lookups are behind.
	signerWarnedAt time.Time

	pools    map[string]*trackedPool // keyed by lowercase pool address or PoolId
	cursor   uint64                  // last block the shared scan has read
	decimals map[string]int
	signers  map[common.Hash]string // lookup cache; each swap keeps its own signer

	nativeUSD  float64
	nativeOK   bool
	nativeAsOf time.Time
	pricePeers []chain.ID
}

type trackedPool struct {
	p          chain.Pool
	o          chain.Orientation
	dec0, dec1 int
	v4, v3     bool

	// favorite: starred, so tracked until unstarred and never screened.
	favorite bool
	// screened: the pair has cleared the feed's bar and stays tracked.
	screened bool
	// frozen: the window has ended. No more events are read, but signers of
	// swaps inside the window are still resolved, so buyers and sellers
	// converge on the window's exact counts on chains too busy to look every
	// signer up in time.
	frozen bool

	// fillFrom..fillTo is the pair's own history still to read: from its
	// creation up to where the shared scan picked it up. The pair is warm, and
	// its figures are published, once fillFrom passes fillTo.
	fillFrom, fillTo uint64
	// partial: history before historyCap was never read.
	partial bool
	// head is the latest block the pair's events have been read to.
	head uint64

	seen map[string]uint64 // applied log identity -> its block

	prices []pricePoint // price trail, in event order
	sqrt   pricePoint   // latest raw sqrt price, concentrated pools

	r0, r1 *big.Int // v2-style reserves
	syncAt uint64

	bal0, bal1    *big.Int // v3 pool token balances
	balancesStale bool

	positions map[[2]int32]*big.Int // v4 liquidity by tick range

	swaps []swapRecord

	dirty   bool
	written *store.Market
}

type pricePoint struct {
	at uint64 // block<<20 | log index: event order
	v  float64
	ok bool
}

type swapRecord struct {
	at     uint64
	tx     common.Hash
	buy    bool    // a buy of the traded token
	quote  float64 // quote tokens traded, whole units
	signer string  // empty until looked up
}

func logOrder(l types.Log) uint64 { return l.BlockNumber<<20 | uint64(l.Index&0xFFFFF) }

func blockOf(at uint64) uint64 { return at >> 20 }

func newTracker(i *Ingester, window, every time.Duration) *tracker {
	return &tracker{
		i: i, window: window, every: every,
		pools:    map[string]*trackedPool{},
		decimals: map[string]int{},
		signers:  map[common.Hash]string{},
	}
}

// newTrackedPool starts a pair with no history left to read; callers that need
// its history set the fill range.
func newTrackedPool(p chain.Pool, o chain.Orientation, dec0, dec1 int) *trackedPool {
	v4 := chain.IsV4PoolID(p.Address)
	v3 := p.Model == chain.ModelCLMM && !v4
	return &trackedPool{
		p: p, o: o, dec0: dec0, dec1: dec1, v4: v4, v3: v3,
		favorite:      p.FavoritedAt != nil,
		screened:      p.ScreenedAt != nil,
		fillFrom:      1,
		seen:          map[string]uint64{},
		positions:     map[[2]int32]*big.Int{},
		balancesStale: v3,
		dirty:         true,
	}
}

func (tp *trackedPool) warm() bool { return tp.fillFrom > tp.fillTo }

// trackLoop runs the tracker for one chain until the context ends.
func (i *Ingester) trackLoop(ctx context.Context) {
	window := i.cfg.TrackWindow
	if window <= 0 {
		return
	}
	every := i.cfg.TrackEvery
	if every <= 0 {
		every = 3 * time.Second
	}
	t := newTracker(i, window, every)
	t.pricePeers = i.nativePricePeers
	t.screen = i.cfg.Screen

	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			t.tick(ctx, time.Now())
		}
	}
}

func (t *tracker) tick(ctx context.Context, now time.Time) {
	t.refresh(ctx, now)
	if len(t.pools) == 0 {
		t.cursor = 0
		if len(t.signers) > 0 {
			t.signers = map[common.Hash]string{}
		}
		return
	}
	if err := t.scan(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.i.log.Warn("market tracker scan failed; retrying next tick", "error", err)
	} else if err == nil {
		t.scannedAt = now
	}
	t.readHistory(ctx)
	t.refreshBalances(ctx)
	t.refreshNativePrice(ctx, now)
	// Screen before looking up traders or publishing, so a removed pair costs
	// no more lookups and no update frame follows its removal frame.
	t.screenPairs(ctx, now)
	t.resolveSigners(ctx, now)
	t.publish(ctx)
	t.expire(now)
}

// blocksIn converts a duration into this chain's blocks.
func (t *tracker) blocksIn(d time.Duration) uint64 {
	return uint64(d / t.i.ch.ExpectedBlockTime())
}

// refresh adds pairs that should be tracked and are not, and picks up pairs
// starred or unstarred since the last tick.
func (t *tracker) refresh(ctx context.Context, now time.Time) {
	pools, err := t.i.store.TrackablePools(ctx, t.i.ch.ID, now.Add(-t.window))
	if err != nil {
		t.i.log.Debug("market tracker could not list pools to track", "error", err)
		return
	}
	listed := make(map[string]chain.Pool, len(pools))
	for _, p := range pools {
		listed[strings.ToLower(p.Address)] = p
	}

	for key, tp := range t.pools {
		p, ok := listed[key]
		tp.favorite = ok && p.FavoritedAt != nil
		if tp.favorite && tp.frozen {
			// A frozen pair stopped reading events. Starred again, it starts over
			// from its own history rather than with a gap.
			delete(t.pools, key)
		}
	}

	for _, p := range pools {
		key := strings.ToLower(p.Address)
		if _, ok := t.pools[key]; ok || p.Token0 == nil || p.Token1 == nil {
			continue
		}
		d0, ok0 := t.decimalsOf(ctx, p.Token0.Address)
		d1, ok1 := t.decimalsOf(ctx, p.Token1.Address)
		if !ok0 || !ok1 {
			continue // try again next tick
		}
		if t.cursor == 0 {
			head := t.i.pool.FleetHead(t.i.ch.ID)
			if head == 0 {
				return
			}
			t.cursor = head
		}
		tp := newTrackedPool(p, chain.Orient(t.i.ch, p.Token0, p.Token1), d0, d1)
		t.planHistory(tp)
		t.pools[key] = tp
	}
}

// planHistory sets the history a joining pair reads on its own: from its
// creation block — its opening price and first liquidity land there — up to
// where the shared scan has read, which the scan will not read again for it.
//
// Rewinding the shared cursor instead, as this once did, re-read every tracked
// pair's range whenever one joined: cheap while pairs joined within seconds of
// creation, unaffordable once a starred pair can be a day old.
func (t *tracker) planHistory(tp *trackedPool) {
	upTo := t.cursor
	tp.head = upTo
	from := tp.p.CreatedBlock
	if from == 0 || from > upTo {
		tp.fillFrom, tp.fillTo = upTo+1, upTo
		return
	}
	if limit := t.blocksIn(historyCap); upTo > limit && from < upTo-limit {
		from = upTo - limit
		tp.partial = true
	}
	tp.fillFrom, tp.fillTo = from, upTo
}

// decimalsOf resolves a token's decimals: native currency is 18, otherwise the
// stored token record, otherwise one eth_call. Cached for the process lifetime.
func (t *tracker) decimalsOf(ctx context.Context, token string) (int, bool) {
	key := strings.ToLower(token)
	if d, ok := t.decimals[key]; ok {
		return d, true
	}
	if chain.IsNative(key) {
		t.decimals[key] = 18
		return 18, true
	}
	if rec, err := t.i.store.GetToken(ctx, t.i.ch.ID, key); err == nil && rec.Decimals != nil {
		t.decimals[key] = *rec.Decimals
		return *rec.Decimals, true
	}
	out, err := t.i.evm.CallContract(ctx, common.HexToAddress(key), []byte{0x31, 0x3c, 0xe5, 0x67}, nil, nil)
	if err != nil || len(out) < 32 {
		return 0, false
	}
	d := new(big.Int).SetBytes(out[:32])
	if !d.IsInt64() || d.Int64() > 36 {
		return 0, false
	}
	t.decimals[key] = int(d.Int64())
	return int(d.Int64()), true
}

// scan reads every live pair's events from the shared cursor towards the head.
func (t *tracker) scan(ctx context.Context) error {
	var live []*trackedPool
	for _, tp := range t.pools {
		if !tp.frozen {
			live = append(live, tp)
		}
	}
	if len(live) == 0 {
		return nil
	}
	head := t.i.pool.FleetHead(t.i.ch.ID)
	if head == 0 || head <= t.cursor {
		return nil
	}
	from, to := t.cursor+1, head
	// A cursor that fell behind (failed scans) catches up in bounded steps.
	if span := t.i.chunk(head-from > 100, head-from); span > 0 && to > from+span-1 {
		to = from + span - 1
	}

	logs, err := t.fetch(ctx, from, to, live)
	if err != nil {
		return err
	}
	t.route(logs, func(tp *trackedPool) bool { return !tp.frozen })
	t.cursor = to
	for _, tp := range live {
		tp.head = to
	}
	return nil
}

// readHistory reads joining pairs' own history, a bounded number of queries per
// tick, oldest blocks first. Every pair still reading shares each query.
func (t *tracker) readHistory(ctx context.Context) {
	for range historyQueriesPerTick {
		var from, last uint64
		found := false
		for _, tp := range t.pools {
			if tp.warm() || tp.frozen {
				continue
			}
			if !found || tp.fillFrom < from {
				from = tp.fillFrom
			}
			if !found || tp.fillTo > last {
				last = tp.fillTo
			}
			found = true
		}
		if !found {
			return
		}
		span := t.i.chunk(true, t.cursor-min(from, t.cursor))
		if span == 0 {
			return
		}
		to := min(from+span-1, last)

		group := map[*trackedPool]bool{}
		var members []*trackedPool
		for _, tp := range t.pools {
			if !tp.warm() && !tp.frozen && tp.fillFrom <= to {
				group[tp] = true
				members = append(members, tp)
			}
		}
		logs, err := t.fetch(ctx, from, to, members)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				t.i.log.Debug("market tracker history read failed; retrying next tick", "error", err)
			}
			return
		}
		// Blocks a member already read, on its own or through the shared scan,
		// come back too; applied logs are remembered, so none counts twice.
		t.route(logs, func(tp *trackedPool) bool { return group[tp] })
		for _, tp := range members {
			tp.fillFrom = to + 1
			tp.dirty = true
		}
	}
}

// fetch reads the tracker's events for a set of pairs over a range, in event
// order: pair contracts by address, v4 pools on the PoolManager by PoolId.
func (t *tracker) fetch(ctx context.Context, from, to uint64, pools []*trackedPool) ([]types.Log, error) {
	var addrs []common.Address
	var ids []common.Hash
	for _, tp := range pools {
		if tp.v4 {
			ids = append(ids, common.HexToHash(tp.p.Address))
		} else {
			addrs = append(addrs, common.HexToAddress(tp.p.Address))
		}
	}

	// Filters are split into groups: the tracked set can be more pairs than
	// some providers accept in one filter.
	const perQuery = 100
	var logs []types.Log
	for len(addrs) > 0 {
		n := min(len(addrs), perQuery)
		l, err := t.i.sweepQuery(ctx, from, to, addrs[:n], [][]common.Hash{pairTopics}, rpcpool.CapLogs)
		if err != nil {
			return nil, err
		}
		logs = append(logs, l...)
		addrs = addrs[n:]
	}
	if manager := t.i.poolManager(); manager != (common.Address{}) {
		for len(ids) > 0 {
			n := min(len(ids), perQuery)
			l, err := t.i.sweepQuery(ctx, from, to, []common.Address{manager},
				[][]common.Hash{v4Topics, ids[:n]}, rpcpool.CapLogs)
			if err != nil {
				return nil, err
			}
			logs = append(logs, l...)
			ids = ids[n:]
		}
	}
	sort.Slice(logs, func(a, b int) bool { return logOrder(logs[a]) < logOrder(logs[b]) })
	return logs, nil
}

// route applies each log to its pair, when accept allows that pair.
func (t *tracker) route(logs []types.Log, accept func(*trackedPool) bool) {
	for _, l := range logs {
		if l.Removed || len(l.Topics) == 0 {
			continue
		}
		key := strings.ToLower(l.Address.Hex())
		if l.Topics[0] == chain.TopicSwapV4 || l.Topics[0] == chain.TopicModifyLiquidity || l.Topics[0] == chain.TopicInitializeV4 {
			if len(l.Topics) < 2 {
				continue
			}
			key = strings.ToLower(l.Topics[1].Hex())
		}
		if tp, ok := t.pools[key]; ok && accept(tp) {
			tp.apply(l)
		}
	}
}

// apply folds one event into a pair's state.
func (tp *trackedPool) apply(l types.Log) {
	id := l.TxHash.Hex() + ":" + big.NewInt(int64(l.Index)).String()
	if _, ok := tp.seen[id]; ok {
		return
	}
	tp.seen[id] = l.BlockNumber
	at := logOrder(l)

	switch l.Topics[0] {
	case chain.TopicInitializeV3:
		if sqrt, err := chain.DecodeInitializeV3(l); err == nil {
			tp.observeSqrt(at, sqrt)
		}
		tp.balancesStale = true

	case chain.TopicInitializeV4:
		if sqrt, err := chain.DecodeInitializeV4Price(l); err == nil {
			tp.observeSqrt(at, sqrt)
		}

	case chain.TopicSyncV2, chain.TopicSyncSolidly:
		if r, err := chain.DecodeSync(l); err == nil {
			// Only the newest Sync sets the current reserves, but every Sync is a
			// point on the price trail: an older one arriving late may be the
			// pair's opening price.
			if at >= tp.syncAt {
				tp.r0, tp.r1, tp.syncAt = r.Reserve0, r.Reserve1, at
			}
			tp.observePrice(at, chain.PriceFromReserves(r.Reserve0, r.Reserve1, tp.dec0, tp.dec1))
		}

	case chain.TopicModifyLiquidity:
		if lc, err := chain.DecodeModifyLiquidityV4(l); err == nil {
			rng := [2]int32{lc.TickLower, lc.TickUpper}
			cur := tp.positions[rng]
			if cur == nil {
				cur = new(big.Int)
			}
			tp.positions[rng] = cur.Add(cur, lc.LiquidityDelta)
			tp.dirty = true
		}

	case topicMintV3, topicBurnV3:
		tp.balancesStale = true
		tp.dirty = true

	case chain.TopicSwapV4, chain.TopicSwapV2, chain.TopicSwapSolidly, chain.TopicSwapV3, chain.TopicSwapPancakeV3:
		var sw chain.Swap
		var err error
		if l.Topics[0] == chain.TopicSwapV4 {
			sw, err = chain.DecodeSwapV4(l)
		} else {
			sw, err = chain.DecodeSwap(l)
		}
		if err != nil {
			return
		}
		if sw.SqrtPriceX96 != nil {
			tp.observeSqrt(at, sw.SqrtPriceX96)
		}
		if tp.v3 {
			tp.balancesStale = true
		}
		quoteRaw, quoteDec := sw.Amount1, tp.dec1
		if !tp.o.BaseIsToken0 {
			quoteRaw, quoteDec = sw.Amount0, tp.dec0
		}
		var quote float64
		if quoteRaw != nil {
			quote = chain.ScaleDown(new(big.Int).Abs(quoteRaw), quoteDec)
		}
		// Swap.Buyer means "received token0": a buy of the traded token only
		// when the traded token is token0.
		tp.swaps = append(tp.swaps, swapRecord{at: at, tx: l.TxHash, buy: sw.Buyer == tp.o.BaseIsToken0, quote: quote})
		tp.dirty = true
	}
}

func (tp *trackedPool) observeSqrt(at uint64, sqrtX96 *big.Int) {
	raw := chain.SqrtX96ToFloat(sqrtX96)
	if raw <= 0 {
		return
	}
	if !tp.sqrt.ok || at >= tp.sqrt.at {
		tp.sqrt = pricePoint{at: at, v: raw, ok: true}
	}
	tp.observePrice(at, raw*raw*math.Pow10(tp.dec0-tp.dec1))
}

// observePrice records token1-per-token0 as the traded token's price in its
// quote, on a trail kept in event order. Events arrive out of order — a pair's
// history is read after the scan has already picked it up — so each point is
// inserted where it belongs.
func (tp *trackedPool) observePrice(at uint64, token1PerToken0 float64) {
	if token1PerToken0 <= 0 || math.IsInf(token1PerToken0, 0) || math.IsNaN(token1PerToken0) {
		return
	}
	price := token1PerToken0
	if !tp.o.BaseIsToken0 {
		price = 1 / token1PerToken0
	}
	n := sort.Search(len(tp.prices), func(k int) bool { return tp.prices[k].at >= at })
	if n < len(tp.prices) && tp.prices[n].at == at {
		tp.prices[n].v = price
	} else {
		tp.prices = slices.Insert(tp.prices, n, pricePoint{at: at, v: price, ok: true})
	}
	tp.dirty = true
}

// cutoff is the last block outside the rolling hour.
func (tp *trackedPool) cutoff(hour uint64) uint64 {
	if tp.head > hour {
		return tp.head - hour
	}
	return 0
}

// accelWindow is the recent slice volume acceleration compares: the last
// minute against the pair's average minute. A minute is short enough to catch a
// pair waking up inside the ten minutes it is tracked, and long enough that one
// block's trades do not decide it.
const accelWindow = time.Minute

// minAccelWindows is how much history a pair needs before the comparison means
// anything: with less, "the last minute against the average minute" is the last
// minute against itself.
const minAccelWindows = 3

// volumeAcceleration compares the volume of the last window with the pair's
// average window over its measured span — since it launched, or the last hour
// for a pair older than that. 3.2 means the last minute traded 3.2 times the
// pair's usual minute; 1 is steady, below 1 is cooling off.
//
// It is a ratio of quote-token amounts, so no dollar price is needed and it is
// reported even when the quote asset has no price. Nil until the pair has
// minAccelWindows of history and some volume to compare against.
func (tp *trackedPool) volumeAcceleration(window, cutoff uint64) (float64, bool) {
	if window == 0 || len(tp.swaps) == 0 || tp.head == 0 {
		return 0, false
	}
	// The measured span starts at the pair's own creation, not at the hour
	// cutoff: for a ten-minute-old pair the hour reaches back before it existed,
	// and dividing by an hour would report every pair as accelerating.
	from := cutoff
	if tp.p.CreatedBlock > from {
		from = tp.p.CreatedBlock
	}
	if tp.head <= from {
		return 0, false
	}
	span := tp.head - from
	if span < uint64(minAccelWindows)*window {
		return 0, false
	}
	recentFrom := tp.head - window

	var total, recent float64
	for _, s := range tp.swaps {
		b := blockOf(s.at)
		if b <= from {
			continue
		}
		total += s.quote
		if b > recentFrom {
			recent += s.quote
		}
	}
	if total <= 0 {
		return 0, false
	}
	perWindow := total / (float64(span) / float64(window))
	if perWindow <= 0 {
		return 0, false
	}
	return recent / perWindow, true
}

// priceChange returns the price at the start of the hour — the last point at
// or before the cutoff, or the first point for a pair younger than that — and
// the latest price.
func (tp *trackedPool) priceChange(cutoff uint64) (open, last float64, ok bool) {
	if len(tp.prices) == 0 {
		return 0, 0, false
	}
	after := sort.Search(len(tp.prices), func(k int) bool { return blockOf(tp.prices[k].at) > cutoff })
	start := tp.prices[0]
	if after > 0 {
		start = tp.prices[after-1]
	}
	return start.v, tp.prices[len(tp.prices)-1].v, start.v > 0
}

// unresolved reports whether any swap inside the hour still has no signer.
func (tp *trackedPool) unresolved(cutoff uint64) bool {
	for _, s := range tp.swaps {
		if s.signer == "" && blockOf(s.at) > cutoff {
			return true
		}
	}
	return false
}

// prune drops what has left the rolling hour: swaps, price points except the
// one that opens the hour, and the identities of logs too old to be read again.
// Only a warm pair is pruned: one still reading its history can be handed an
// old log again.
func (tp *trackedPool) prune(cutoff uint64) {
	keep := tp.swaps[:0]
	for _, s := range tp.swaps {
		if blockOf(s.at) > cutoff {
			keep = append(keep, s)
		}
	}
	tp.swaps = keep
	if after := sort.Search(len(tp.prices), func(k int) bool { return blockOf(tp.prices[k].at) > cutoff }); after > 1 {
		tp.prices = slices.Clone(tp.prices[after-1:])
	}
	for id, block := range tp.seen {
		if block <= cutoff {
			delete(tp.seen, id)
		}
	}
}

// resolveSigners looks up who signed pending swaps, in batches. Uniswap v2/v3
// events name a recipient and v4 events name the calling contract — usually a
// router either way — so the transaction signer is the only reliable identity
// for a distinct trader.
//
// Lookups are budgeted per tick. They cost one request each — Robinhood's
// endpoint meters every element of a batch, and answered 429 after 150 — and a
// busy chain makes more swaps than a public endpoint can look up. Unbudgeted,
// one tick spent minutes here and took the rate budget pool discovery needs.
//
// The budget is shared round-robin across pairs: one transaction from each pair
// in turn, starting from a different pair each tick. Taking the oldest swaps
// first let one busy pair's hour of trades hold every other pair at zero. Swaps
// outside the rolling hour are not looked up; they no longer count.
func (t *tracker) resolveSigners(ctx context.Context, now time.Time) {
	hour := t.blocksIn(time.Hour)
	order := make([]*trackedPool, 0, len(t.pools))
	for _, tp := range t.pools {
		order = append(order, tp)
	}
	sort.Slice(order, func(a, b int) bool { return order[a].p.CreatedBlock < order[b].p.CreatedBlock })

	assign := func() {
		for _, tp := range order {
			for n := range tp.swaps {
				if tp.swaps[n].signer != "" {
					continue
				}
				if signer, ok := t.signers[tp.swaps[n].tx]; ok {
					tp.swaps[n].signer = signer
					tp.dirty = true
				}
			}
		}
	}
	assign()

	queues := make([][]common.Hash, len(order))
	queued := map[common.Hash]bool{}
	unresolved := 0
	for n, tp := range order {
		cutoff := tp.cutoff(hour)
		for _, s := range tp.swaps {
			if s.signer != "" || queued[s.tx] || blockOf(s.at) <= cutoff {
				continue
			}
			queued[s.tx] = true
			queues[n] = append(queues[n], s.tx)
		}
		unresolved += len(queues[n])
	}
	if unresolved == 0 {
		return
	}

	budget := t.signerBudget()
	need := roundRobin(queues, budget, t.rotation)
	t.rotation++
	backlog := unresolved - len(need)

	// No deadline here: the budget already bounds this. A rate limiter refuses
	// outright a wait that would pass a deadline, so a tick-length deadline on a
	// busy endpoint failed every batch — after spending the tokens of the
	// lookups ahead of it — and no trader was ever counted.
	for h, a := range t.i.evm.TransactionSenders(ctx, need) {
		t.signers[h] = strings.ToLower(a.Hex())
	}
	if backlog > 0 && now.Sub(t.signerWarnedAt) > time.Minute {
		t.signerWarnedAt = now
		t.i.log.Warn("market tracker: trader lookups are behind; buyers/sellers fill in as they resolve",
			"unresolvedSwaps", backlog, "lookupsPerTick", budget)
	}
	assign()
}

// signerBudget is how many signer lookups one tick may make: a share of the
// chain's HTTP request rate over the tick, leaving the rest for discovery.
func (t *tracker) signerBudget() int {
	const share = 0.4
	n := int(t.i.pool.ChainRate(t.i.ch.ID) * t.every.Seconds() * share)
	return min(max(n, 5), 500)
}

// roundRobin takes up to budget items, one from each queue in turn, starting at
// queue first (modulo the count).
func roundRobin(queues [][]common.Hash, budget, first int) []common.Hash {
	var out []common.Hash
	if len(queues) == 0 {
		return out
	}
	first %= len(queues)
	for depth := 0; len(out) < budget; depth++ {
		took := false
		for k := range queues {
			q := queues[(first+k)%len(queues)]
			if depth < len(q) && len(out) < budget {
				out = append(out, q[depth])
				took = true
			}
		}
		if !took {
			break
		}
	}
	return out
}

const balanceMulticallABI = `[
 {"name":"aggregate3","type":"function","stateMutability":"payable",
  "inputs":[{"name":"calls","type":"tuple[]","components":[
    {"name":"target","type":"address"},{"name":"allowFailure","type":"bool"},{"name":"callData","type":"bytes"}]}],
  "outputs":[{"name":"returnData","type":"tuple[]","components":[
    {"name":"success","type":"bool"},{"name":"returnData","type":"bytes"}]}]},
 {"name":"balanceOf","type":"function","stateMutability":"view",
  "inputs":[{"name":"owner","type":"address"}],"outputs":[{"name":"","type":"uint256"}]}
]`

var balanceABI = func() abi.ABI {
	a, err := abi.JSON(strings.NewReader(balanceMulticallABI))
	if err != nil {
		panic("ingest: bad built-in ABI: " + err.Error())
	}
	return a
}()

type call3 struct {
	Target       common.Address
	AllowFailure bool
	CallData     []byte
}

// refreshBalances reads the token balances of v3 pairs whose balances changed,
// in one multicall. A v3 pool contract holds both of its tokens, so its
// balances are its liquidity — no tick arithmetic needed.
func (t *tracker) refreshBalances(ctx context.Context) {
	if t.i.ch.Multicall3 == "" {
		return
	}
	var targets []*trackedPool
	var calls []call3
	for _, tp := range t.pools {
		if !tp.v3 || !tp.balancesStale || tp.frozen {
			continue
		}
		owner := common.HexToAddress(tp.p.Address)
		data, err := balanceABI.Pack("balanceOf", owner)
		if err != nil {
			continue
		}
		targets = append(targets, tp)
		calls = append(calls,
			call3{Target: common.HexToAddress(tp.p.Token0.Address), AllowFailure: true, CallData: data},
			call3{Target: common.HexToAddress(tp.p.Token1.Address), AllowFailure: true, CallData: data})
	}
	if len(calls) == 0 {
		return
	}
	payload, err := balanceABI.Pack("aggregate3", calls)
	if err != nil {
		return
	}
	raw, err := t.i.evm.CallContract(ctx, common.HexToAddress(t.i.ch.Multicall3), payload, nil, nil)
	if err != nil {
		return // stale balances stay flagged; next tick retries
	}
	vals, err := balanceABI.Unpack("aggregate3", raw)
	if err != nil || len(vals) == 0 {
		return
	}
	results, ok := vals[0].([]struct {
		Success    bool   `json:"success"`
		ReturnData []byte `json:"returnData"`
	})
	if !ok || len(results) != len(calls) {
		return
	}
	for n, tp := range targets {
		r0, r1 := results[2*n], results[2*n+1]
		if !r0.Success || !r1.Success || len(r0.ReturnData) < 32 || len(r1.ReturnData) < 32 {
			continue
		}
		tp.bal0 = new(big.Int).SetBytes(r0.ReturnData[:32])
		tp.bal1 = new(big.Int).SetBytes(r1.ReturnData[:32])
		tp.balancesStale = false
		tp.dirty = true
	}
}

// refreshNativePrice reads the chain's native-token price, falling back to a
// chain with the same native asset. Robinhood Chain and Arbitrum pay gas in ETH
// but have no reference pool of their own; ETH's price is the same asset's price
// wherever it is read, so Ethereum's or Base's serves. A chain paying gas in a
// stablecoin needs neither.
func (t *tracker) refreshNativePrice(ctx context.Context, now time.Time) {
	if now.Sub(t.nativeAsOf) < 30*time.Second {
		return
	}
	t.nativeAsOf = now
	if t.i.ch.NativeIsStable {
		// Gas is paid in a dollar stablecoin (Arc): no reference pool needed.
		t.nativeUSD, t.nativeOK = 1, true
		return
	}
	for _, id := range append([]chain.ID{t.i.ch.ID}, t.pricePeers...) {
		if usd, ok, err := t.i.store.NativePrice(ctx, id); err == nil && ok && usd > 0 {
			t.nativeUSD, t.nativeOK = usd, true
			return
		}
	}
	t.nativeOK = false
}

// quoteUSD is the dollar price of a quote token, when it is known: a stable is
// one dollar, the native currency and its wrapper are the native price.
// Anything else is unknown, and the dollar columns stay empty rather than
// guessed.
func (t *tracker) quoteUSD(tok *chain.Token) (float64, bool) {
	if tok == nil {
		return 0, false
	}
	for _, s := range t.i.ch.Stables {
		if strings.EqualFold(s, tok.Address) {
			return 1, true
		}
	}
	if chain.IsNative(tok.Address) || strings.EqualFold(t.i.ch.WrappedNative, tok.Address) {
		return t.nativeUSD, t.nativeOK
	}
	return 0, false
}

// metrics computes the columns from a pair's state over its rolling hour.
func (t *tracker) metrics(tp *trackedPool) store.Market {
	var m store.Market
	cutoff := tp.cutoff(t.blocksIn(time.Hour))

	buyers, sellers := map[string]struct{}{}, map[string]struct{}{}
	pending := map[common.Hash]struct{}{}
	var volumeQuote float64
	for _, s := range tp.swaps {
		if blockOf(s.at) <= cutoff {
			continue
		}
		volumeQuote += s.quote
		switch {
		case s.signer == "":
			pending[s.tx] = struct{}{}
		case s.buy:
			buyers[s.signer] = struct{}{}
		default:
			sellers[s.signer] = struct{}{}
		}
	}
	b, s, p := len(buyers), len(sellers), len(pending)
	m.Buyers1h, m.Sellers1h, m.TradersPending = &b, &s, &p

	if open, last, ok := tp.priceChange(cutoff); ok {
		change := (last/open - 1) * 100
		m.PriceChange1h = &change
	}

	usd, ok := t.quoteUSD(tp.o.Quote)
	if !ok {
		return m
	}
	vol := volumeQuote * usd
	m.Volume1hUSD = &vol
	if accel, ok := tp.volumeAcceleration(t.blocksIn(accelWindow), cutoff); ok {
		m.VolAccel = &accel
	}

	if base, quote, known := tp.holdings(); known && len(tp.prices) > 0 {
		liq := quote*usd + base*tp.prices[len(tp.prices)-1].v*usd
		m.LiquidityUSD = &liq
	}
	return m
}

// holdings returns the traded token and quote token held by the pair, in whole
// tokens, when they can be established.
func (tp *trackedPool) holdings() (base, quote float64, known bool) {
	var a0, a1 float64
	switch {
	case tp.v4:
		if tp.partial {
			// Positions opened before the history that was read are missing.
			return 0, 0, false
		}
		if len(tp.positions) == 0 {
			// No liquidity has been added through the PoolManager. A hooked pool
			// may keep liquidity in its hook instead, so that is unknown; an
			// unhooked pool genuinely holds nothing.
			if tp.p.Hooks != "" {
				return 0, 0, false
			}
			return 0, 0, true
		}
		if !tp.sqrt.ok {
			return 0, 0, false
		}
		for rng, L := range tp.positions {
			if L.Sign() <= 0 {
				continue
			}
			lf, _ := new(big.Float).SetInt(L).Float64()
			x0, x1 := chain.PositionAmounts(lf, rng[0], rng[1], tp.sqrt.v)
			a0 += x0
			a1 += x1
		}
		a0 /= math.Pow10(tp.dec0)
		a1 /= math.Pow10(tp.dec1)
	case tp.v3:
		if tp.bal0 == nil || tp.bal1 == nil {
			return 0, 0, false
		}
		a0, a1 = chain.ScaleDown(tp.bal0, tp.dec0), chain.ScaleDown(tp.bal1, tp.dec1)
	default:
		if tp.r0 == nil || tp.r1 == nil {
			return 0, 0, false
		}
		a0, a1 = chain.ScaleDown(tp.r0, tp.dec0), chain.ScaleDown(tp.r1, tp.dec1)
	}
	if tp.o.BaseIsToken0 {
		return a0, a1, true
	}
	return a1, a0, true
}

// screenPairs judges each pair that has reached its screening time. A pair
// whose liquidity or volume falls short of the bar is removed from the feed and
// dropped from tracking; the rest are tracked for the remainder of the window.
// Favorites are not judged: starring a pair is choosing to keep it.
//
// A verdict waits for figures that are only late, not unknowable: a scan that
// has not yet read past the screening time, history still being read, a quote
// priced in native currency before that price has loaded, a v3 pool's balances
// still being read. Past a grace period it judges on what is known, and a
// dollar figure that cannot be established counts as not clearing the bar — a
// pair quoted in an asset with no dollar price cannot be shown to clear it.
func (t *tracker) screenPairs(ctx context.Context, now time.Time) {
	if t.screen.After <= 0 {
		return
	}
	const grace = 2 * time.Minute
	for key, tp := range t.pools {
		if tp.screened || tp.frozen || tp.favorite || !tp.warm() {
			continue
		}
		due := tp.p.CreatedAt.Add(t.screen.After)
		if now.Before(due) || t.scannedAt.Before(due) {
			continue
		}
		late := now.After(due.Add(grace))
		if !late && (t.quotePending(tp.o.Quote) || (tp.v3 && tp.balancesStale)) {
			continue
		}

		keep := t.clearsBar(t.metrics(tp))
		at, err := t.i.store.ScreenPool(ctx, tp.p.ChainID, tp.p.Address, keep)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				delete(t.pools, key)
			}
			continue // retried next tick
		}
		outcome := "kept"
		if keep {
			tp.screened = true
			tp.p.ScreenedAt = &at
			tp.dirty = true
		} else {
			outcome = "removed"
			delete(t.pools, key)
			p := tp.p
			p.ScreenedAt, p.RemovedAt = &at, &at
			t.i.sink.PoolRemoved(ctx, p)
		}
		obs.PairsScreened.WithLabelValues(string(t.i.ch.ID), outcome).Inc()
	}
}

// clearsBar applies the screening thresholds. Unknown figures do not clear.
func (t *tracker) clearsBar(m store.Market) bool {
	liq := m.LiquidityUSD != nil && *m.LiquidityUSD >= t.screen.MinLiquidityUSD
	vol := m.Volume1hUSD != nil && *m.Volume1hUSD >= t.screen.MinVolumeUSD
	if t.screen.KeepIf == "either" {
		return liq || vol
	}
	return liq && vol
}

// quotePending reports a quote asset that has a dollar price whose value has
// not loaded yet: the native currency or its wrapper before the native price
// is known. Other unpriced quotes are not pending; they are unknowable.
func (t *tracker) quotePending(tok *chain.Token) bool {
	if tok == nil || t.nativeOK {
		return false
	}
	return chain.IsNative(tok.Address) || strings.EqualFold(t.i.ch.WrappedNative, tok.Address)
}

// publish writes the pairs whose figures changed and pushes them to clients. A
// pair still reading its history is not published: its figures are partial.
func (t *tracker) publish(ctx context.Context) {
	for _, tp := range t.pools {
		if !tp.dirty || !tp.warm() {
			continue
		}
		m := t.metrics(tp)
		tp.dirty = false
		if tp.written != nil && sameMarket(*tp.written, m) {
			continue
		}
		if err := t.i.store.UpdateTrackedMarket(ctx, tp.p.ChainID, tp.p.Address, m); err != nil {
			tp.dirty = true // retry next tick
			continue
		}
		written := m
		tp.written = &written
		if p, err := t.i.store.GetPool(ctx, tp.p.ChainID, tp.p.Address); err == nil {
			t.i.sink.PoolMarketUpdated(ctx, p)
		}
	}
}

// expire prunes what has left each live pair's hour, freezes pairs past their
// window that are not favorites, and drops frozen pairs once their trader
// counts are complete or the drain time is up. Their last figures stand.
func (t *tracker) expire(now time.Time) {
	hour := t.blocksIn(time.Hour)
	for key, tp := range t.pools {
		if !tp.frozen && tp.warm() {
			if cutoff := tp.cutoff(hour); cutoff > 0 {
				tp.prune(cutoff)
			}
		}
		if tp.favorite {
			continue
		}
		age := now.Sub(tp.p.CreatedAt)
		if age <= t.window {
			continue
		}
		tp.frozen = true
		if !tp.unresolved(tp.cutoff(hour)) || age > t.window+signerDrain {
			delete(t.pools, key)
		}
	}
	// Every tracked pair is frozen: nothing is scanned, and the next pair to
	// arrive starts the cursor from the head.
	live := false
	for _, tp := range t.pools {
		live = live || !tp.frozen
	}
	if !live {
		t.cursor = 0
	}
	if len(t.signers) > 100_000 {
		t.signers = map[common.Hash]string{}
	}
}

func sameMarket(a, b store.Market) bool {
	eqF := func(x, y *float64) bool {
		if x == nil || y == nil {
			return x == y
		}
		return math.Abs(*x-*y) <= 1e-9*math.Max(1, math.Abs(*y))
	}
	eqI := func(x, y *int) bool {
		if x == nil || y == nil {
			return x == y
		}
		return *x == *y
	}
	return eqF(a.LiquidityUSD, b.LiquidityUSD) && eqF(a.Volume1hUSD, b.Volume1hUSD) &&
		eqF(a.VolAccel, b.VolAccel) &&
		eqF(a.PriceChange1h, b.PriceChange1h) && eqI(a.Buyers1h, b.Buyers1h) && eqI(a.Sellers1h, b.Sellers1h) &&
		eqI(a.TradersPending, b.TradersPending)
}

// poolManager returns the chain's Uniswap v4 PoolManager, if one is configured.
func (i *Ingester) poolManager() common.Address {
	for _, f := range i.ch.Factories {
		if f.Event == chain.EventInitializeV4 {
			return common.HexToAddress(f.Address)
		}
	}
	return common.Address{}
}
