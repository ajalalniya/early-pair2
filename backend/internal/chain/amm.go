package chain

import (
	"math"
	"math/big"
)

// Price and liquidity arithmetic for the market columns.
//
// Everything here produces display figures — a price, a dollar liquidity
// value — so float64 is used deliberately. Amounts that reach a transaction
// never pass through this file; they stay big.Int end to end.

var q96 = new(big.Float).SetInt(new(big.Int).Lsh(big.NewInt(1), 96))

// SqrtX96ToFloat converts a Q64.96 square-root price to a plain float: the
// square root of token1 per token0, in raw base units.
func SqrtX96ToFloat(sqrtX96 *big.Int) float64 {
	if sqrtX96 == nil || sqrtX96.Sign() <= 0 {
		return 0
	}
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(sqrtX96), q96).Float64()
	return f
}

// PriceFromSqrtX96 returns token1 per token0 in whole tokens.
func PriceFromSqrtX96(sqrtX96 *big.Int, dec0, dec1 int) float64 {
	s := SqrtX96ToFloat(sqrtX96)
	if s == 0 {
		return 0
	}
	return s * s * math.Pow10(dec0-dec1)
}

// PriceFromReserves returns token1 per token0 in whole tokens, from a v2-style
// pool's reserves.
func PriceFromReserves(r0, r1 *big.Int, dec0, dec1 int) float64 {
	a := ScaleDown(r0, dec0)
	b := ScaleDown(r1, dec1)
	if a <= 0 {
		return 0
	}
	return b / a
}

// ScaleDown converts a base-unit amount to whole tokens.
func ScaleDown(v *big.Int, decimals int) float64 {
	if v == nil {
		return 0
	}
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(v), new(big.Float).SetFloat64(math.Pow10(decimals))).Float64()
	return f
}

// Tick bounds for Uniswap v3 and v4.
const (
	MinTick = -887272
	MaxTick = 887272
)

// SqrtPriceAtTick returns sqrt(1.0001^tick), the raw square-root price at a
// tick boundary.
func SqrtPriceAtTick(tick int32) float64 {
	if tick < MinTick {
		tick = MinTick
	}
	if tick > MaxTick {
		tick = MaxTick
	}
	return math.Pow(1.0001, float64(tick)/2)
}

// PositionAmounts returns the raw token0 and token1 amounts held by liquidity
// L between two ticks, at the current raw square-root price. These are the
// standard concentrated-liquidity formulas: below the range a position is all
// token0, above it all token1, and inside it holds both.
func PositionAmounts(liquidity float64, tickLower, tickUpper int32, sqrtPrice float64) (amount0, amount1 float64) {
	if liquidity <= 0 || tickLower >= tickUpper || sqrtPrice <= 0 {
		return 0, 0
	}
	sa := SqrtPriceAtTick(tickLower)
	sb := SqrtPriceAtTick(tickUpper)
	switch {
	case sqrtPrice <= sa:
		amount0 = liquidity * (sb - sa) / (sa * sb)
	case sqrtPrice >= sb:
		amount1 = liquidity * (sb - sa)
	default:
		amount0 = liquidity * (sb - sqrtPrice) / (sqrtPrice * sb)
		amount1 = liquidity * (sqrtPrice - sa)
	}
	return amount0, amount1
}
