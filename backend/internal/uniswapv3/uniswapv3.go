// Package uniswapv3 quotes and encodes swaps against Uniswap v3 pools.
//
// A v3 pool's price lives in ticks, and its liquidity is whatever the range
// orders around the current price provide, so there is no closed-form price to
// compute off pool state the way a constant-product pair has. Quotes therefore
// come from Uniswap's QuoterV2, which runs the swap inside the pool and reverts
// with the result: every tick crossed and the fee in force are included.
// Nothing here approximates a v3 price.
//
// Swaps go through SwapRouter02. Its exactInputSingle carries no deadline — the
// router takes one through multicall(deadline, calls) instead — so that is how
// a swap is wrapped here, and a transaction left unsigned in a wallet cannot be
// mined at a price that has since moved.
package uniswapv3

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"github.com/earlypair/backend/internal/rpcpool"
)

func mustABI(s string) abi.ABI {
	a, err := abi.JSON(strings.NewReader(s))
	if err != nil {
		panic("uniswapv3: bad built-in ABI: " + err.Error())
	}
	return a
}

var quoterABI = mustABI(`[{"name":"quoteExactInputSingle","type":"function","stateMutability":"nonpayable",
 "inputs":[{"name":"params","type":"tuple","components":[
   {"name":"tokenIn","type":"address"},{"name":"tokenOut","type":"address"},
   {"name":"amountIn","type":"uint256"},{"name":"fee","type":"uint24"},
   {"name":"sqrtPriceLimitX96","type":"uint160"}]}],
 "outputs":[{"name":"amountOut","type":"uint256"},{"name":"sqrtPriceX96After","type":"uint160"},
   {"name":"initializedTicksCrossed","type":"uint32"},{"name":"gasEstimate","type":"uint256"}]}]`)

var routerABI = mustABI(`[
 {"name":"exactInputSingle","type":"function","stateMutability":"payable",
  "inputs":[{"name":"params","type":"tuple","components":[
    {"name":"tokenIn","type":"address"},{"name":"tokenOut","type":"address"},
    {"name":"fee","type":"uint24"},{"name":"recipient","type":"address"},
    {"name":"amountIn","type":"uint256"},{"name":"amountOutMinimum","type":"uint256"},
    {"name":"sqrtPriceLimitX96","type":"uint160"}]}],
  "outputs":[{"name":"amountOut","type":"uint256"}]},
 {"name":"multicall","type":"function","stateMutability":"payable",
  "inputs":[{"name":"deadline","type":"uint256"},{"name":"data","type":"bytes[]"}],
  "outputs":[{"name":"results","type":"bytes[]"}]}]`)

type quoteParams struct {
	TokenIn           common.Address
	TokenOut          common.Address
	AmountIn          *big.Int
	Fee               *big.Int
	SqrtPriceLimitX96 *big.Int
}

type exactInputSingleParams struct {
	TokenIn           common.Address
	TokenOut          common.Address
	Fee               *big.Int
	Recipient         common.Address
	AmountIn          *big.Int
	AmountOutMinimum  *big.Int
	SqrtPriceLimitX96 *big.Int
}

// Swap describes an exact-input single-pool swap.
type Swap struct {
	TokenIn  common.Address
	TokenOut common.Address
	// Fee is the pool's fee as a raw uint24: hundredths of a bip, so 3000 is
	// 0.30%. It is part of the pool's identity, not a setting — the router finds
	// the pool from (tokenIn, tokenOut, fee), and a wrong value addresses a
	// different pool or none at all.
	Fee       *big.Int
	Recipient common.Address
	AmountIn  *big.Int
	MinOut    *big.Int // enforced by the router: the swap reverts below it
	Deadline  *big.Int // unix seconds, carried by multicall
}

// maxUint128 bounds the amounts the quoter and router accept in practice.
var maxUint128 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))

func validFee(fee *big.Int) bool {
	return fee != nil && fee.Sign() > 0 && fee.Cmp(big.NewInt(1_000_000)) < 0
}

// Quote asks the QuoterV2 for the output of an exact-input single-pool swap.
// The sqrtPriceLimitX96 of zero means "no limit": the quote reflects the whole
// price move the trade causes, which is what price impact is measured from.
func Quote(ctx context.Context, evm *rpcpool.EVM, quoter, tokenIn, tokenOut common.Address, fee, amountIn *big.Int) (amountOut *big.Int, gasEstimate uint64, err error) {
	if amountIn == nil || amountIn.Sign() <= 0 || amountIn.Cmp(maxUint128) > 0 {
		return nil, 0, errors.New("uniswapv3: amount out of range")
	}
	if !validFee(fee) {
		return nil, 0, errors.New("uniswapv3: pool fee unknown")
	}
	data, err := quoterABI.Pack("quoteExactInputSingle", quoteParams{
		TokenIn: tokenIn, TokenOut: tokenOut, AmountIn: amountIn, Fee: fee, SqrtPriceLimitX96: big.NewInt(0),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("uniswapv3: encode quote: %w", err)
	}
	out, err := evm.CallContract(ctx, quoter, data, nil, nil)
	if err != nil {
		return nil, 0, err
	}
	vals, err := quoterABI.Unpack("quoteExactInputSingle", out)
	if err != nil || len(vals) < 4 {
		return nil, 0, fmt.Errorf("uniswapv3: decode quote: %v", err)
	}
	amountOut, _ = vals[0].(*big.Int)
	gas, _ := vals[3].(*big.Int)
	if amountOut == nil {
		return nil, 0, errors.New("uniswapv3: quote returned no amount")
	}
	if gas != nil && gas.IsUint64() {
		gasEstimate = gas.Uint64()
	}
	return amountOut, gasEstimate, nil
}

// EncodeSwap builds the SwapRouter02 call: exactInputSingle inside a multicall
// that carries the deadline.
func EncodeSwap(s Swap) ([]byte, error) {
	if s.AmountIn == nil || s.AmountIn.Sign() <= 0 || s.AmountIn.Cmp(maxUint128) > 0 {
		return nil, errors.New("uniswapv3: amountIn out of range")
	}
	if s.MinOut == nil || s.MinOut.Sign() < 0 {
		return nil, errors.New("uniswapv3: minOut out of range")
	}
	if !validFee(s.Fee) {
		return nil, errors.New("uniswapv3: pool fee unknown")
	}
	if s.Deadline == nil || s.Deadline.Sign() <= 0 {
		return nil, errors.New("uniswapv3: deadline required")
	}
	inner, err := routerABI.Pack("exactInputSingle", exactInputSingleParams{
		TokenIn: s.TokenIn, TokenOut: s.TokenOut, Fee: s.Fee, Recipient: s.Recipient,
		AmountIn: s.AmountIn, AmountOutMinimum: s.MinOut, SqrtPriceLimitX96: big.NewInt(0),
	})
	if err != nil {
		return nil, fmt.Errorf("uniswapv3: encode swap: %w", err)
	}
	data, err := routerABI.Pack("multicall", s.Deadline, [][]byte{inner})
	if err != nil {
		return nil, fmt.Errorf("uniswapv3: encode multicall: %w", err)
	}
	return data, nil
}
