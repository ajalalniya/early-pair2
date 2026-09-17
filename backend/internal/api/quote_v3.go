package api

import (
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/rpcpool"
	"github.com/earlypair/backend/internal/uniswapv3"
)

// Uniswap v3 quoting and transaction building.
//
// Prices come from the QuoterV2, which runs the swap inside the pool, so ticks
// crossed and the fee tier are accounted for; swaps go through SwapRouter02
// with the deadline carried by its multicall. Paying with native currency is
// not offered here: on v3 that needs the router's wrap-and-refund sequence,
// and the one chain this was built for (Arc) has no wrapped native at all.

// v3RouterOverheadGas covers what the quoter's swap estimate leaves out:
// the transfer in, the multicall dispatch and the transfer out.
const v3RouterOverheadGas = 90_000

// isV3Pool reports whether a pool is a Uniswap v3 pool of the deployment this
// chain's contracts serve. A v3 fork with its own factory (PancakeSwap v3) is
// not one: its pools are not in Uniswap's factory, so Uniswap's quoter would
// price a pool that does not exist.
func isV3Pool(ch chain.Chain, p chain.Pool) bool {
	return !isV4Pool(p) && p.Model == chain.ModelCLMM &&
		strings.EqualFold(p.Dex, ch.UniswapV3.DexName())
}

// v3Ready reports whether a v3 pool on this chain can be traded here, writing
// the error when it cannot.
func (s *Server) v3Ready(w http.ResponseWriter, ch chain.Chain, p chain.Pool) bool {
	if ch.UniswapV3 == nil || ch.UniswapV3.Quoter == "" || ch.UniswapV3.SwapRouter == "" {
		writeError(w, http.StatusNotImplemented, "v3_not_configured",
			fmt.Sprintf("%s pools are listed on this chain but trading them is not configured", p.Dex))
		return false
	}
	if p.Token0 == nil || p.Token1 == nil || p.Token0.Symbol == "" || p.Token1.Symbol == "" {
		writeError(w, http.StatusConflict, "not_enriched",
			"this pool's tokens are not resolved yet; try again in a moment")
		return false
	}
	if p.FeeTierBps == nil {
		// The fee tier is part of the pool's identity: the router finds the pool
		// from (tokenIn, tokenOut, fee). Guessing a tier would address a
		// different pool, so refuse instead.
		writeError(w, http.StatusConflict, "fee_tier_unknown",
			"this pool's fee tier is not known, so its swap cannot be addressed")
		return false
	}
	return true
}

// v3Fee is the pool's fee as the router wants it: a raw uint24 in hundredths
// of a bip, where the stored tier is in basis points.
func v3Fee(p chain.Pool) *big.Int {
	return big.NewInt(int64(*p.FeeTierBps) * 100)
}

func (s *Server) quoteSwapV3(w http.ResponseWriter, r *http.Request, req swapQuoteRequest, ch chain.Chain, p chain.Pool,
	amountIn *big.Int, slippage int, gated bool, reason string) {
	if !s.v3Ready(w, ch, p) {
		return
	}
	inToken, outToken, _, _ := tradeSides(chain.Orient(ch, p.Token0, p.Token1), req.Side, nil, nil)
	if req.PayNative {
		writeError(w, http.StatusBadRequest, "native_unsupported",
			"paying with the native currency is not supported on Uniswap v3 pools here")
		return
	}

	ctx := r.Context()
	evm := s.pool.EVM(ch.ID)
	quoter := common.HexToAddress(ch.UniswapV3.Quoter)
	tokenIn, tokenOut := common.HexToAddress(inToken.Address), common.HexToAddress(outToken.Address)
	fee := v3Fee(p)

	amountOut, swapGas, err := uniswapv3.Quote(ctx, evm, quoter, tokenIn, tokenOut, fee, amountIn)
	if err != nil {
		if rpcpool.IsExecutionReverted(err) {
			// The pool cannot fill it: no liquidity in range at this price, or
			// the trade would cross further than the pool has orders for.
			writeError(w, http.StatusUnprocessableEntity, "no_route",
				"this pool cannot fill that trade (no liquidity in range at its current price)")
			return
		}
		s.log.Warn("v3 quote failed", "pool", p.Address, "error", err)
		writeError(w, http.StatusBadGateway, "quote_failed", "the quote could not be fetched from the chain; try again")
		return
	}
	if amountOut.Sign() <= 0 {
		writeError(w, http.StatusBadRequest, "no_output",
			"that input is too small to produce any output at this pool's liquidity")
		return
	}

	minReceived := new(big.Int).Div(new(big.Int).Mul(amountOut, big.NewInt(int64(10000-slippage))), big.NewInt(10000))

	// Price impact against a quote a thousand times smaller, which pays the same
	// fee but barely moves the price: the gap between them is this trade's impact.
	impact, rate := 0, trimFloat(0)
	if small := new(big.Int).Div(amountIn, big.NewInt(1000)); small.Sign() > 0 {
		if smallOut, _, err := uniswapv3.Quote(ctx, evm, quoter, tokenIn, tokenOut, fee, small); err == nil && smallOut.Sign() > 0 {
			impact = priceImpactBps(amountIn, amountOut, small, smallOut)
			rate = spotRate(small, smallOut, inToken.Decimals, outToken.Decimals)
		}
	}
	if rate == "0" {
		rate = spotRate(amountIn, amountOut, inToken.Decimals, outToken.Decimals)
	}

	resp := QuoteResponse{
		Legs: []Leg{{
			Step:  "1",
			Label: fmt.Sprintf("Swap %s → %s on %s", inToken.Symbol, outToken.Symbol, p.Dex),
			Value: formatUnits(amountIn, inToken.Decimals) + " " + inToken.Symbol,
		}},
		AmountIn:       amountIn.String(),
		AmountOut:      amountOut.String(),
		MinReceived:    minReceived.String(),
		PriceImpactBps: impact,
		Rate:           fmt.Sprintf("1 %s = %s %s", outToken.Symbol, rate, inToken.Symbol),
		ExpiresAt:      time.Now().Add(30 * time.Second),
		Gated:          gated,
		GateReason:     reason,
	}
	resp.EstGas = swapGas + v3RouterOverheadGas
	if gasPrice, err := evm.GasPrice(ctx); err == nil && gasPrice != nil {
		cost := new(big.Int).Mul(gasPrice, new(big.Int).SetUint64(resp.EstGas))
		resp.GasPriceWei = gasPrice.String()
		resp.EstGasNative = formatUnits(cost, 18)
		if nativeUSD, ok := s.nativePriceUSD(ctx, ch); ok {
			usd := parseFloat(resp.EstGasNative) * nativeUSD
			resp.EstGasUSD = &usd
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) buildSwapV3(w http.ResponseWriter, r *http.Request, req txBuildRequest, ch chain.Chain, p chain.Pool,
	amountIn, minOut *big.Int) {
	if !s.v3Ready(w, ch, p) {
		return
	}
	if req.PayNative {
		writeError(w, http.StatusBadRequest, "native_unsupported",
			"paying with the native currency is not supported on Uniswap v3 pools here")
		return
	}
	inToken, outToken, _, _ := tradeSides(chain.Orient(ch, p.Token0, p.Token1), req.Side, nil, nil)

	deadline := req.DeadlineSecs
	if deadline <= 0 || deadline > 3600 {
		deadline = 300
	}
	expires := time.Now().Add(time.Duration(deadline) * time.Second)

	ctx := r.Context()
	evm := s.pool.EVM(ch.ID)
	router := common.HexToAddress(ch.UniswapV3.SwapRouter)
	from := common.HexToAddress(req.From)
	tokenIn := common.HexToAddress(inToken.Address)

	data, err := uniswapv3.EncodeSwap(uniswapv3.Swap{
		TokenIn: tokenIn, TokenOut: common.HexToAddress(outToken.Address), Fee: v3Fee(p),
		Recipient: from, AmountIn: amountIn, MinOut: minOut, Deadline: big.NewInt(expires.Unix()),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "encode_failed", err.Error())
		return
	}

	// SwapRouter02 pulls the input itself, so one allowance to the router is the
	// whole approval story here — no Permit2 leg, unlike v4.
	steps, err := s.approvalSteps(ctx, ch, p, from, inToken, amountIn, expires)
	if err != nil {
		// A shortfall that could not be read is caught by the simulation below.
		s.log.Debug("v3 build could not read the allowance", "pool", p.Address, "error", err)
	}
	if steps == nil {
		steps = []UnsignedTx{}
	}

	swapTx := UnsignedTx{
		ChainID: ch.ID, To: router.Hex(), Data: hexutil.Encode(data), Value: "0",
		Kind: "swap", Label: fmt.Sprintf("Swap %s → %s on %s", inToken.Symbol, outToken.Symbol, p.Dex),
	}

	// Simulated only when nothing is pending: a swap simulated before its
	// approval is mined reverts for the wrong reason.
	if len(steps) == 0 {
		if _, err := evm.Simulate(ctx, from, router, data, nil, nil); err != nil {
			switch {
			case isInsufficientFunds(err):
				writeError(w, http.StatusUnprocessableEntity, "insufficient_funds",
					fmt.Sprintf("this wallet does not hold enough %s for the trade", inToken.Symbol))
				return
			case rpcpool.IsExecutionReverted(err):
				reason := rpcpool.DecodeRevert(err)
				if reason == "" {
					reason = "the swap reverts in simulation: the price moved past your minimum, the balance is short, or the token blocks the transfer"
				}
				writeError(w, http.StatusUnprocessableEntity, "simulation_reverted", reason)
				return
			default:
				swapTx.Label += " (not simulated)"
			}
		}
		if gas, err := evm.EstimateGas(ctx, from, router, data, nil); err == nil {
			swapTx.Gas = gas + gas/5 // a fifth of headroom for ticks crossed between now and mining
		}
	}
	steps = append(steps, swapTx)

	writeJSON(w, http.StatusOK, map[string]any{
		"steps":     steps,
		"expiresAt": expires,
		"note":      "sign these in order with your own wallet; this service holds no keys and cannot sign",
	})
}
