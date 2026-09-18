package clmm

import (
	"math"
	"math/big"
	"testing"

	"github.com/earlypair/backend/internal/chain"
)

func L(n int64) *big.Int { return big.NewInt(n) }

// Two overlapping positions: one from -200 to 200, one from 0 to 400. Between
// 0 and 200 both are active, so that range holds the sum.
func TestBinsAccumulateOverlappingRanges(t *testing.T) {
	ticks := map[int32]*big.Int{
		-200: L(100), // +100 entering at -200
		0:    L(50),  // +50 entering at 0
		200:  L(-100),
		400:  L(-50),
	}
	// The pool says 150 is active, and the price sits between 0 and 200.
	got := bins(ticks, 100, 20, L(150), sqrtAt(100), 18, 18, true)

	want := []struct {
		lo, hi int32
		l      float64
		active bool
	}{
		{-200, 0, 100, false},
		{0, 200, 150, true},
		{200, 400, 50, false},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d bins, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		b := got[i]
		if b.TickLower != w.lo || b.TickUpper != w.hi || b.Liquidity != w.l || b.Active != w.active {
			t.Errorf("bin %d = %d..%d L=%g active=%v, want %d..%d L=%g active=%v",
				i, b.TickLower, b.TickUpper, b.Liquidity, b.Active, w.lo, w.hi, w.l, w.active)
		}
	}
}

// The pool's own active liquidity is the anchor: when the scan missed ranges
// below the window, every bin shifts so the active one matches what the pool
// reports, rather than the curve sagging by the missing amount.
func TestBinsAnchorToThePoolsOwnLiquidity(t *testing.T) {
	ticks := map[int32]*big.Int{0: L(10), 200: L(-10)}
	got := bins(ticks, 100, 20, L(1000), sqrtAt(100), 18, 18, true)
	if len(got) != 1 {
		t.Fatalf("got %d bins, want 1", len(got))
	}
	if got[0].Liquidity != 1000 {
		t.Errorf("active bin L=%g, want the pool's own 1000", got[0].Liquidity)
	}
}

// A price that has run past every range: nothing is active, and the bins that
// exist still describe where the liquidity sits.
func TestBinsWithThePriceOutsideEveryRange(t *testing.T) {
	ticks := map[int32]*big.Int{0: L(10), 200: L(-10)}
	got := bins(ticks, 5000, 20, L(0), sqrtAt(5000), 18, 18, true)
	if len(got) != 1 {
		t.Fatalf("got %d bins, want 1", len(got))
	}
	if got[0].Active {
		t.Error("a bin claims a price that is outside it")
	}
	if got[0].Liquidity != 10 {
		t.Errorf("bin L=%g, want 10 from the ticks themselves", got[0].Liquidity)
	}
}

func TestBinsNeedTwoBoundaries(t *testing.T) {
	if got := bins(map[int32]*big.Int{0: L(10)}, 0, 20, L(10), sqrtAt(0), 18, 18, true); got != nil {
		t.Errorf("one tick is not a range, got %+v", got)
	}
}

// The word a tick falls in, including below zero, where truncating division
// would land a tick in the word above the one that holds it.
func TestWordOfFloorsBelowZero(t *testing.T) {
	cases := []struct {
		tick, spacing int32
		want          int16
	}{
		{0, 200, 0},
		{51200, 200, 1},
		{-1, 200, -1},
		{-395804, 200, -8},
		{chain.MaxTick, 200, 17},
		{chain.MinTick, 200, -18},
	}
	for _, c := range cases {
		if got := wordOf(c.tick, c.spacing); got != c.want {
			t.Errorf("wordOf(%d, %d) = %d, want %d", c.tick, c.spacing, got, c.want)
		}
	}
}

// Prices bound each bin, and inverting for a pair quoted the other way round
// swaps the ends rather than reporting an upside-down range.
func TestPriceRangeOrientsBothWays(t *testing.T) {
	lo, hi := priceRange(0, 200, 18, 18, true)
	if lo != 1 || math.Abs(hi-math.Pow(1.0001, 200)) > 1e-9 {
		t.Errorf("price range %g..%g, want 1..1.0001^200", lo, hi)
	}
	ilo, ihi := priceRange(0, 200, 18, 18, false)
	if ihi != 1 || math.Abs(ilo-1/math.Pow(1.0001, 200)) > 1e-9 {
		t.Errorf("inverted range %g..%g", ilo, ihi)
	}
}

// sqrtAt is the raw Q64.96 square-root price at a tick, as a pool stores it.
func sqrtAt(tick int32) *big.Int {
	q96 := new(big.Float).SetFloat64(math.Pow(2, 96))
	f := new(big.Float).Mul(big.NewFloat(chain.SqrtPriceAtTick(tick)), q96)
	out, _ := f.Int(nil)
	return out
}
