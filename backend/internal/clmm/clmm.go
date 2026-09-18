// Package clmm reads the liquidity distribution of a concentrated-liquidity
// pool — Uniswap v3, and v4 through its StateView — and turns it into the bins
// a depth chart draws.
//
// The distribution is read from pool state, not from history. Replaying every
// Mint, Burn and ModifyLiquidity since a pool's creation would cost hundreds of
// log queries on a chain that serves 500-block ranges, and would cost more
// every day the pool lives. The state holds the same answer in a bounded number
// of reads: a bitmap says which ticks have liquidity, and each of those ticks
// carries the amount that starts or ends there. Both are batched through
// Multicall3, so a whole distribution is two round trips.
//
// Everything reported is what the pool says about itself. Where a chain has no
// StateView deployed, v4 depth is reported as unavailable rather than guessed
// from the pool's token balances, which would describe a v2 curve the pool does
// not have.
package clmm

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/rpcpool"
)

func mustABI(s string) abi.ABI {
	a, err := abi.JSON(strings.NewReader(s))
	if err != nil {
		panic("clmm: bad built-in ABI: " + err.Error())
	}
	return a
}

var (
	// A v3 pool answers for itself.
	poolV3 = mustABI(`[
	  {"name":"slot0","inputs":[],"outputs":[
	    {"name":"sqrtPriceX96","type":"uint160"},{"name":"tick","type":"int24"},
	    {"name":"observationIndex","type":"uint16"},{"name":"observationCardinality","type":"uint16"},
	    {"name":"observationCardinalityNext","type":"uint16"},{"name":"feeProtocol","type":"uint8"},
	    {"name":"unlocked","type":"bool"}],"stateMutability":"view","type":"function"},
	  {"name":"liquidity","inputs":[],"outputs":[{"type":"uint128"}],"stateMutability":"view","type":"function"},
	  {"name":"tickSpacing","inputs":[],"outputs":[{"type":"int24"}],"stateMutability":"view","type":"function"},
	  {"name":"tickBitmap","inputs":[{"type":"int16"}],"outputs":[{"type":"uint256"}],"stateMutability":"view","type":"function"},
	  {"name":"ticks","inputs":[{"type":"int24"}],"outputs":[
	    {"name":"liquidityGross","type":"uint128"},{"name":"liquidityNet","type":"int128"},
	    {"name":"feeGrowthOutside0X128","type":"uint256"},{"name":"feeGrowthOutside1X128","type":"uint256"},
	    {"name":"tickCumulativeOutside","type":"int56"},{"name":"secondsPerLiquidityOutsideX128","type":"uint160"},
	    {"name":"secondsOutside","type":"uint32"},{"name":"initialized","type":"bool"}],
	   "stateMutability":"view","type":"function"}]`)

	// A v4 pool has no contract; StateView reads the PoolManager's state by id.
	stateView = mustABI(`[
	  {"name":"getSlot0","inputs":[{"type":"bytes32"}],"outputs":[
	    {"name":"sqrtPriceX96","type":"uint160"},{"name":"tick","type":"int24"},
	    {"name":"protocolFee","type":"uint24"},{"name":"lpFee","type":"uint24"}],
	   "stateMutability":"view","type":"function"},
	  {"name":"getLiquidity","inputs":[{"type":"bytes32"}],"outputs":[{"type":"uint128"}],"stateMutability":"view","type":"function"},
	  {"name":"getTickBitmap","inputs":[{"type":"bytes32"},{"type":"int16"}],"outputs":[{"type":"uint256"}],"stateMutability":"view","type":"function"},
	  {"name":"getTickLiquidity","inputs":[{"type":"bytes32"},{"type":"int24"}],"outputs":[
	    {"name":"liquidityGross","type":"uint128"},{"name":"liquidityNet","type":"int128"}],
	   "stateMutability":"view","type":"function"}]`)

	multicall3 = mustABI(`[
	  {"name":"aggregate3","inputs":[{"components":[
	    {"name":"target","type":"address"},{"name":"allowFailure","type":"bool"},{"name":"callData","type":"bytes"}],
	    "name":"calls","type":"tuple[]"}],
	   "outputs":[{"components":[{"name":"success","type":"bool"},{"name":"returnData","type":"bytes"}],
	    "name":"returnData","type":"tuple[]"}],"stateMutability":"payable","type":"function"}]`)
)

type call3 struct {
	Target       common.Address `json:"target"`
	AllowFailure bool           `json:"allowFailure"`
	CallData     []byte         `json:"callData"`
}

type result3 struct {
	Success    bool   `json:"success"`
	ReturnData []byte `json:"returnData"`
}

// Bin is one step of the depth chart: the liquidity active between two ticks.
type Bin struct {
	TickLower int32 `json:"tickLower"`
	TickUpper int32 `json:"tickUpper"`
	// PriceLower and PriceUpper bound the bin in quote tokens per traded
	// token, decimals applied and oriented the way the pair is displayed.
	PriceLower float64 `json:"priceLower"`
	PriceUpper float64 `json:"priceUpper"`
	// Liquidity is L, the pool's own unit. It is what makes bins comparable
	// with each other; the amounts say what it is worth.
	Liquidity float64 `json:"liquidity"`
	// Amount0 and Amount1 are the tokens this bin's liquidity holds at the
	// pool's current price, in whole tokens.
	Amount0 float64 `json:"amount0"`
	Amount1 float64 `json:"amount1"`
	// Active marks the bin holding the current price: the liquidity a trade
	// touches first.
	Active bool `json:"active"`
}

// Distribution is a pool's liquidity across price, as a depth chart draws it.
type Distribution struct {
	Tick        int32   `json:"tick"`
	Price       float64 `json:"price"`
	TickSpacing int32   `json:"tickSpacing"`
	// Liquidity is L active at the current tick, which the bins should agree
	// with: a mismatch means the bitmap scan did not reach far enough.
	Liquidity float64 `json:"liquidity"`
	Bins      []Bin   `json:"bins"`
	// Truncated says the scan hit its word budget before running out of
	// initialised ticks, so the chart's edges are the scan's, not the pool's.
	Truncated bool `json:"truncated"`
}

// Reader is what a pool family must answer: the calls are batched, so each
// method returns calldata rather than making the call. Its methods are
// unexported, so the two implementations here are the only ones — a caller
// picks one, it cannot invent a third that reports liquidity from elsewhere.
type Reader interface {
	target() common.Address
	slot0() []byte
	liquidity() []byte
	bitmap(word int16) []byte
	tick(t int32) []byte
	decodeSlot0(b []byte) (sqrtX96 *big.Int, tick int32, err error)
	decodeLiquidity(b []byte) (*big.Int, error)
	decodeBitmap(b []byte) (*big.Int, error)
	decodeTickNet(b []byte) (*big.Int, error)
}

// wordBudget bounds a scan: 2 x this many bitmap words around the current
// price. Each word covers 256 tick spacings — for a 1% fee pool, several
// multiples of the current price — so this reaches far past where any depth
// chart is readable, while keeping one distribution to a bounded cost.
const wordBudget = 24

// Read builds the distribution of a v3 pool (its own contract) or a v4 pool
// (through StateView), in two multicalls: one for the pool's price, liquidity
// and spacing plus the bitmap words around it, one for the ticks those words
// mark as initialised.
func Read(ctx context.Context, evm *rpcpool.EVM, multicall common.Address, r Reader, dec0, dec1 int, baseIsToken0 bool) (*Distribution, error) {
	if multicall == (common.Address{}) {
		return nil, errors.New("clmm: this chain has no multicall3 configured")
	}
	spacing, sqrtX96, tick, active, err := readState(ctx, evm, multicall, r)
	if err != nil {
		return nil, err
	}
	if spacing <= 0 {
		return nil, errors.New("clmm: the pool reported no tick spacing")
	}

	ticks, truncated, err := readInitialisedTicks(ctx, evm, multicall, r, tick, spacing)
	if err != nil {
		return nil, err
	}

	d := &Distribution{
		Tick: tick, TickSpacing: spacing,
		Liquidity: bigFloat(active),
		Price:     chain.PriceFromSqrtX96(sqrtX96, dec0, dec1),
		Truncated: truncated,
	}
	if !baseIsToken0 && d.Price != 0 {
		d.Price = 1 / d.Price
	}
	d.Bins = bins(ticks, tick, spacing, active, sqrtX96, dec0, dec1, baseIsToken0)
	return d, nil
}

func readState(ctx context.Context, evm *rpcpool.EVM, multicall common.Address, r Reader) (spacing int32, sqrtX96 *big.Int, tick int32, active *big.Int, err error) {
	res, err := aggregate(ctx, evm, multicall, []call3{
		{Target: r.target(), AllowFailure: true, CallData: r.slot0()},
		{Target: r.target(), AllowFailure: true, CallData: r.liquidity()},
	})
	if err != nil {
		return 0, nil, 0, nil, err
	}
	if len(res) < 2 || !res[0].Success || !res[1].Success {
		return 0, nil, 0, nil, errors.New("clmm: the pool did not answer for its price and liquidity")
	}
	sqrtX96, tick, err = r.decodeSlot0(res[0].ReturnData)
	if err != nil {
		return 0, nil, 0, nil, err
	}
	active, err = r.decodeLiquidity(res[1].ReturnData)
	if err != nil {
		return 0, nil, 0, nil, err
	}
	spacing, err = spacingOf(ctx, evm, multicall, r)
	return spacing, sqrtX96, tick, active, err
}

// readInitialisedTicks walks the bitmap outward from the current price and
// reads the net liquidity of every tick it marks.
func readInitialisedTicks(ctx context.Context, evm *rpcpool.EVM, multicall common.Address, r Reader,
	tick, spacing int32) (map[int32]*big.Int, bool, error) {
	centre := wordOf(tick, spacing)

	// The window around the price, plus the two words at the ends of the tick
	// space. A full-range position — the usual shape for a launch — puts its
	// ticks at those ends, thousands of words away, and without them the range
	// holding the current price never forms at all.
	lowWord := wordOf(chain.MinTick, spacing)
	highWord := wordOf(chain.MaxTick, spacing)
	seen := map[int16]bool{}
	words := make([]int16, 0, 2*wordBudget+3)
	addWord := func(w int16) {
		if w < lowWord || w > highWord || seen[w] {
			return
		}
		seen[w] = true
		words = append(words, w)
	}
	addWord(lowWord)
	for w := -wordBudget; w <= wordBudget; w++ {
		addWord(centre + int16(w))
	}
	addWord(highWord)
	calls := make([]call3, 0, len(words))
	for _, w := range words {
		calls = append(calls, call3{Target: r.target(), AllowFailure: true, CallData: r.bitmap(w)})
	}
	res, err := aggregate(ctx, evm, multicall, calls)
	if err != nil {
		return nil, false, err
	}

	var initialised []int32
	truncated := false
	for i, w := range words {
		if i >= len(res) || !res[i].Success {
			continue
		}
		bm, err := r.decodeBitmap(res[i].ReturnData)
		if err != nil || bm.Sign() == 0 {
			continue
		}
		// Liquidity sitting on the window's own edge means the pool reaches
		// past the scan: the chart's edge is the budget's, not the pool's.
		if w == centre-wordBudget || w == centre+wordBudget {
			truncated = true
		}
		for bit := 0; bit < 256; bit++ {
			if bm.Bit(bit) == 1 {
				initialised = append(initialised, (int32(w)*256+int32(bit))*spacing)
			}
		}
	}
	if len(initialised) == 0 {
		return map[int32]*big.Int{}, truncated, nil
	}

	calls = calls[:0]
	for _, t := range initialised {
		calls = append(calls, call3{Target: r.target(), AllowFailure: true, CallData: r.tick(t)})
	}
	res, err = aggregate(ctx, evm, multicall, calls)
	if err != nil {
		return nil, truncated, err
	}
	out := make(map[int32]*big.Int, len(initialised))
	for i, t := range initialised {
		if i >= len(res) || !res[i].Success {
			continue
		}
		net, err := r.decodeTickNet(res[i].ReturnData)
		if err != nil || net.Sign() == 0 {
			continue
		}
		out[t] = net
	}
	return out, truncated, nil
}

// bins turns net liquidity per tick into the liquidity active in each range.
//
// Sweeping upward from the lowest initialised tick is all it takes: a tick's
// net is what is added when it is crossed upward, so the running total is the
// liquidity active above it. The result is then shifted so the range holding
// the current price reports the pool's own active liquidity — the one figure
// the pool states outright. Without that anchor, liquidity sitting below the
// scan's window would tilt the whole curve; with it, only the shape is read
// from the ticks and the level comes from the pool.
func bins(ticks map[int32]*big.Int, current, spacing int32, active, sqrtX96 *big.Int,
	dec0, dec1 int, baseIsToken0 bool) []Bin {
	if len(ticks) < 2 {
		return nil // one boundary is not a range
	}
	bounds := make([]int32, 0, len(ticks))
	for t := range ticks {
		bounds = append(bounds, t)
	}
	sortTicks(bounds)

	running := new(big.Int)
	raw := make([]*big.Int, 0, len(bounds)-1)
	for i := 0; i < len(bounds)-1; i++ {
		running = new(big.Int).Add(running, ticks[bounds[i]])
		raw = append(raw, running)
	}

	// The shift that makes the current range agree with the pool.
	shift := new(big.Int)
	for i, L := range raw {
		if bounds[i] <= current && current < bounds[i+1] {
			shift = new(big.Int).Sub(active, L)
			break
		}
	}

	sqrt := chain.SqrtX96ToFloat(sqrtX96)
	out := make([]Bin, 0, len(raw))
	for i, L := range raw {
		lo, hi := bounds[i], bounds[i+1]
		l := new(big.Int).Add(L, shift)
		if l.Sign() <= 0 {
			continue // an empty range draws nothing
		}
		lf := bigFloat(l)
		a0, a1 := chain.PositionAmounts(lf, lo, hi, sqrt)
		b := Bin{
			TickLower: lo, TickUpper: hi, Liquidity: lf,
			Amount0: a0 / pow10f(dec0), Amount1: a1 / pow10f(dec1),
			Active: lo <= current && current < hi,
		}
		b.PriceLower, b.PriceUpper = priceRange(lo, hi, dec0, dec1, baseIsToken0)
		out = append(out, b)
	}
	return out
}

// wordOf is the bitmap word a tick falls in, at this spacing.
func wordOf(tick, spacing int32) int16 {
	compressed := tick / spacing
	if tick < 0 && tick%spacing != 0 {
		compressed--
	}
	return int16(compressed >> 8)
}

func priceRange(lo, hi int32, dec0, dec1 int, baseIsToken0 bool) (float64, float64) {
	p := func(t int32) float64 {
		s := chain.SqrtPriceAtTick(t)
		raw := s * s * pow10f(dec0) / pow10f(dec1)
		if raw == 0 {
			return 0
		}
		if baseIsToken0 {
			return raw
		}
		return 1 / raw
	}
	a, b := p(lo), p(hi)
	if a > b {
		a, b = b, a
	}
	return a, b
}

func aggregate(ctx context.Context, evm *rpcpool.EVM, multicall common.Address, calls []call3) ([]result3, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	data, err := multicall3.Pack("aggregate3", calls)
	if err != nil {
		return nil, fmt.Errorf("clmm: encode multicall: %w", err)
	}
	raw, err := evm.CallContract(ctx, multicall, data, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("clmm: multicall failed: %w", err)
	}
	vals, err := multicall3.Unpack("aggregate3", raw)
	if err != nil || len(vals) == 0 {
		return nil, fmt.Errorf("clmm: decode multicall: %v", err)
	}
	rows, ok := vals[0].([]struct {
		Success    bool   `json:"success"`
		ReturnData []byte `json:"returnData"`
	})
	if !ok {
		return nil, errors.New("clmm: unexpected multicall result")
	}
	out := make([]result3, len(rows))
	for i, r := range rows {
		out[i] = result3{Success: r.Success, ReturnData: r.ReturnData}
	}
	return out, nil
}

func bigFloat(v *big.Int) float64 {
	if v == nil {
		return 0
	}
	f, _ := new(big.Float).SetInt(v).Float64()
	return f
}

func pow10f(d int) float64 {
	p := 1.0
	for range d {
		p *= 10
	}
	return p
}

func sortTicks(t []int32) {
	for i := 1; i < len(t); i++ {
		for j := i; j > 0 && t[j] < t[j-1]; j-- {
			t[j], t[j-1] = t[j-1], t[j]
		}
	}
}

func sortBins(b []Bin) {
	for i := 1; i < len(b); i++ {
		for j := i; j > 0 && b[j].TickLower < b[j-1].TickLower; j-- {
			b[j], b[j-1] = b[j-1], b[j]
		}
	}
}
