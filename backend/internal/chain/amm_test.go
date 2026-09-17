package chain

import (
	"math"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func near(a, b, tol float64) bool {
	if b == 0 {
		return math.Abs(a) <= tol
	}
	return math.Abs(a-b)/math.Abs(b) <= tol
}

func TestPriceFromSqrtX96(t *testing.T) {
	one := new(big.Int).Lsh(big.NewInt(1), 96) // sqrt price 1 → raw price 1
	if p := PriceFromSqrtX96(one, 18, 18); !near(p, 1, 1e-12) {
		t.Errorf("equal decimals: price %v, want 1", p)
	}
	// Raw price 1 between an 18-decimal token0 and a 6-decimal token1 is
	// 1e12 token1 per token0 in whole tokens.
	if p := PriceFromSqrtX96(one, 18, 6); !near(p, 1e12, 1e-9) {
		t.Errorf("18/6 decimals: price %v, want 1e12", p)
	}
	two := new(big.Int).Lsh(big.NewInt(2), 96) // sqrt 2 → price 4
	if p := PriceFromSqrtX96(two, 18, 18); !near(p, 4, 1e-12) {
		t.Errorf("sqrt 2: price %v, want 4", p)
	}
}

func TestPriceFromReserves(t *testing.T) {
	r0 := new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	r1 := new(big.Int).Mul(big.NewInt(2500), big.NewInt(1_000_000))
	if p := PriceFromReserves(r0, r1, 18, 6); !near(p, 2.5, 1e-12) {
		t.Errorf("price %v, want 2.5 token1 per token0", p)
	}
}

func TestPositionAmountsRegimes(t *testing.T) {
	const L = 1e18
	// Inside a symmetric range at price 1, the position holds equal raw amounts.
	a0, a1 := PositionAmounts(L, -600, 600, 1)
	if a0 <= 0 || a1 <= 0 || !near(a0, a1, 0.01) {
		t.Errorf("in range: %v / %v, want both sides and roughly equal", a0, a1)
	}
	// Price below the range: all token0.
	b0, b1 := PositionAmounts(L, 1000, 2000, SqrtPriceAtTick(500))
	if b0 <= 0 || b1 != 0 {
		t.Errorf("below range: %v / %v, want only token0", b0, b1)
	}
	// Price above the range: all token1.
	c0, c1 := PositionAmounts(L, -2000, -1000, SqrtPriceAtTick(0))
	if c0 != 0 || c1 <= 0 {
		t.Errorf("above range: %v / %v, want only token1", c0, c1)
	}
	// Continuity at the lower edge: just inside matches the all-token0 formula.
	sa := SqrtPriceAtTick(1000)
	edge0, _ := PositionAmounts(L, 1000, 2000, sa*1.0000001)
	below0, _ := PositionAmounts(L, 1000, 2000, sa)
	if !near(edge0, below0, 1e-5) {
		t.Errorf("discontinuity at the range edge: %v vs %v", edge0, below0)
	}
}

func TestDecodeAerodromeSwapAndSync(t *testing.T) {
	sender := common.HexToAddress("0x00000000000000000000000000000000000000a1")
	to := common.HexToAddress("0x00000000000000000000000000000000000000b2")
	data := make([]byte, 128)
	big.NewInt(0).FillBytes(data[0:32])
	big.NewInt(100).FillBytes(data[32:64])
	big.NewInt(40).FillBytes(data[64:96])
	big.NewInt(0).FillBytes(data[96:128])

	sw, err := DecodeSwap(types.Log{
		Topics: []common.Hash{TopicSwapSolidly, common.BytesToHash(sender.Bytes()), common.BytesToHash(to.Bytes())},
		Data:   data,
	})
	if err != nil {
		t.Fatalf("DecodeSwap(aerodrome): %v", err)
	}
	if !sw.Buyer || sw.Amount0.Cmp(big.NewInt(-40)) != 0 {
		t.Errorf("aerodrome swap decoded as buyer=%v amount0=%s", sw.Buyer, sw.Amount0)
	}

	sync := make([]byte, 64)
	big.NewInt(7).FillBytes(sync[0:32])
	big.NewInt(9).FillBytes(sync[32:64])
	r, err := DecodeSync(types.Log{Topics: []common.Hash{TopicSyncSolidly}, Data: sync})
	if err != nil || r.Reserve0.Int64() != 7 || r.Reserve1.Int64() != 9 {
		t.Errorf("aerodrome sync: %+v err=%v", r, err)
	}
}

func TestDecodePancakeV3SwapCarriesPrice(t *testing.T) {
	data := make([]byte, 9*32)
	neg := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(5))
	neg.FillBytes(data[0:32]) // pool paid out 5 token0: a buy
	big.NewInt(10).FillBytes(data[32:64])
	sqrt := new(big.Int).Lsh(big.NewInt(3), 96)
	sqrt.FillBytes(data[64:96])

	sw, err := DecodeSwap(types.Log{
		Topics: []common.Hash{TopicSwapPancakeV3, {}, {}},
		Data:   data,
	})
	if err != nil {
		t.Fatalf("DecodeSwap(pancake v3): %v", err)
	}
	if !sw.Buyer || sw.SqrtPriceX96 == nil || sw.SqrtPriceX96.Cmp(sqrt) != 0 {
		t.Errorf("pancake v3 swap: buyer=%v sqrt=%v", sw.Buyer, sw.SqrtPriceX96)
	}
}

func TestDecodeModifyLiquidityV4(t *testing.T) {
	data := make([]byte, 128)
	neg := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(600))
	neg.FillBytes(data[0:32]) // tickLower -600
	big.NewInt(600).FillBytes(data[32:64])
	big.NewInt(12345).FillBytes(data[64:96])

	id := common.HexToHash("0xabc1")
	lc, err := DecodeModifyLiquidityV4(types.Log{Topics: []common.Hash{TopicModifyLiquidity, id}, Data: data})
	if err != nil {
		t.Fatalf("DecodeModifyLiquidityV4: %v", err)
	}
	if lc.TickLower != -600 || lc.TickUpper != 600 || lc.LiquidityDelta.Int64() != 12345 {
		t.Errorf("decoded %+v", lc)
	}
}
