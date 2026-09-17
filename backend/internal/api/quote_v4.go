package api

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/rpcpool"
	"github.com/earlypair/backend/internal/uniswapv4"
)

// Uniswap v4 quoting and transaction building.
//
// A v4 pool is quoted by Uniswap's V4Quoter, which runs the swap inside the
// PoolManager — the pool's hook, its fee in force and every tick crossed — and
// traded through the Universal Router. Paying native currency needs no approval;
// paying a token goes through Permit2, as two plain approval transactions the
// wallet shows before the swap.

// router gas beyond the quoter's swap estimate: settle, take, command dispatch.
const v4RouterOverheadGas = 110_000

func isV4Pool(p chain.Pool) bool { return chain.IsV4PoolID(p.Address) }

// v4Key resolves and caches a pool's PoolKey from its Initialize event.
func (s *Server) v4Key(ctx context.Context, ch chain.Chain, p chain.Pool) (uniswapv4.PoolKey, error) {
	cacheKey := string(ch.ID) + ":" + strings.ToLower(p.Address)
	if k, ok := s.v4Keys.Load(cacheKey); ok {
		return k.(uniswapv4.PoolKey), nil
	}
	var manager common.Address
	for _, f := range ch.Factories {
		if f.Event == chain.EventInitializeV4 {
			manager = common.HexToAddress(f.Address)
		}
	}
	if manager == (common.Address{}) {
		return uniswapv4.PoolKey{}, errors.New("no v4 PoolManager is configured for this chain")
	}
	k, err := uniswapv4.ResolveKey(ctx, s.pool.EVM(ch.ID), manager, common.HexToHash(p.Address), p.CreatedBlock)
	if err != nil {
		return uniswapv4.PoolKey{}, err
	}
	s.v4Keys.Store(cacheKey, k)
	return k, nil
}

// v4Ready reports whether a v4 pool on this chain can be traded here, writing
// the error when it cannot.
func (s *Server) v4Ready(w http.ResponseWriter, ch chain.Chain, p chain.Pool) bool {
	if ch.UniswapV4 == nil || ch.UniswapV4.Quoter == "" || ch.UniswapV4.UniversalRouter == "" {
		writeError(w, http.StatusNotImplemented, "v4_not_configured",
			"Uniswap v4 trading is not configured for this chain")
		return false
	}
	if p.Token0 == nil || p.Token1 == nil || p.Token0.Symbol == "" || p.Token1.Symbol == "" {
		writeError(w, http.StatusConflict, "not_enriched",
			"this pool's tokens are not resolved yet; try again in a moment")
		return false
	}
	return true
}

func sameAddress(token *chain.Token, a common.Address) bool {
	return token != nil && common.HexToAddress(token.Address) == a
}

func (s *Server) quoteSwapV4(w http.ResponseWriter, r *http.Request, req swapQuoteRequest, ch chain.Chain, p chain.Pool,
	amountIn *big.Int, slippage int, gated bool, reason string) {
	if !s.v4Ready(w, ch, p) {
		return
	}
	inToken, outToken, _, _ := tradeSides(chain.Orient(ch, p.Token0, p.Token1), req.Side, nil, nil)
	native, ok := payNativeFor(w, ch, req.Side, req.PayNative, inToken)
	if !ok {
		return
	}
	ctx := r.Context()
	key, err := s.v4Key(ctx, ch, p)
	if err != nil {
		s.log.Warn("v4 pool key unavailable", "pool", p.Address, "error", err)
		writeError(w, http.StatusBadGateway, "pool_key_unavailable",
			"this pool's parameters could not be read from the chain; try again in a moment")
		return
	}
	zeroForOne := sameAddress(inToken, key.Currency0)
	if !zeroForOne && !sameAddress(inToken, key.Currency1) {
		writeError(w, http.StatusConflict, "pool_mismatch", "this pool's stored tokens do not match its on-chain key")
		return
	}

	evm := s.pool.EVM(ch.ID)
	quoter := common.HexToAddress(ch.UniswapV4.Quoter)
	amountOut, swapGas, err := uniswapv4.Quote(ctx, evm, quoter, key, zeroForOne, amountIn)
	if err != nil {
		if rpcpool.IsExecutionReverted(err) {
			// The pool cannot fill it: not enough liquidity in range, or a hook
			// refusing the swap. The quoter's revert is the pool's own answer.
			writeError(w, http.StatusUnprocessableEntity, "no_route",
				"this pool cannot fill that trade (too little liquidity, or its hook refuses it)")
			return
		}
		s.log.Warn("v4 quote failed", "pool", p.Address, "error", err)
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
	// fee and hook but moves the price negligibly: the gap is the trade's impact.
	impact, rate := 0, trimFloat(0)
	if small := new(big.Int).Div(amountIn, big.NewInt(1000)); small.Sign() > 0 {
		if smallOut, _, err := uniswapv4.Quote(ctx, evm, quoter, key, zeroForOne, small); err == nil && smallOut.Sign() > 0 {
			impact = priceImpactBps(amountIn, amountOut, small, smallOut)
			rate = spotRate(small, smallOut, inToken.Decimals, outToken.Decimals)
		}
	}
	if rate == "0" {
		rate = spotRate(amountIn, amountOut, inToken.Decimals, outToken.Decimals)
	}

	legs := []Leg{{
		Step:  "1",
		Label: fmt.Sprintf("Swap %s → %s on Uniswap v4", inToken.Symbol, outToken.Symbol),
		Value: formatUnits(amountIn, inToken.Decimals) + " " + inToken.Symbol,
	}}
	if native {
		legs = nativeLegs(ch, amountIn, inToken, outToken, " on Uniswap v4")
	}
	if key.Hooks != (common.Address{}) {
		legs = append(legs, Leg{Step: "·", Label: "Pool hook (runs on this swap, not audited)", Value: shortAddr(key.Hooks.Hex())})
	}

	resp := QuoteResponse{
		Legs:           legs,
		AmountIn:       amountIn.String(),
		AmountOut:      amountOut.String(),
		MinReceived:    minReceived.String(),
		PriceImpactBps: impact,
		Rate:           fmt.Sprintf("1 %s = %s %s", outToken.Symbol, rate, inToken.Symbol),
		ExpiresAt:      time.Now().Add(30 * time.Second),
		Gated:          gated,
		GateReason:     reason,
	}
	resp.EstGas = swapGas + v4RouterOverheadGas
	if native {
		resp.EstGas += 45_000 // WRAP_ETH: the router's WETH deposit
	}
	if gasPrice, err := evm.GasPrice(ctx); err == nil && gasPrice != nil {
		cost := new(big.Int).Mul(gasPrice, new(big.Int).SetUint64(resp.EstGas))
		resp.GasPriceWei = gasPrice.String()
		resp.EstGasNative = formatUnits(cost, 18)
		if native, ok := s.nativePriceUSD(ctx, ch); ok {
			usd := parseFloat(resp.EstGasNative) * native
			resp.EstGasUSD = &usd
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// permit2Validity is how long a Permit2 approval stays usable. Uniswap's own
// default, and short enough that an approval nobody revokes lapses on its own.
const permit2Validity = 30 * 24 * time.Hour

// maxUint160 bounds a Permit2 allowance.
var maxUint160 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 160), big.NewInt(1))

func capUint160(v *big.Int) *big.Int {
	if v.Cmp(maxUint160) > 0 {
		return new(big.Int).Set(maxUint160)
	}
	return v
}

// approvalAmount is what an approval step should authorise: the wallet's whole
// balance of the token, so repeated trades in it need no further approval,
// never less than the trade being built. Falls back to the trade's amount when
// the balance cannot be read, which only costs an extra approval later.
func approvalAmount(ctx context.Context, s *Server, ch chain.Chain, token, owner common.Address, amountIn *big.Int) *big.Int {
	balance, _, err := s.tokenBalance(ctx, ch.ID, token, owner)
	if err != nil || balance == nil || balance.Cmp(amountIn) < 0 {
		return amountIn
	}
	return balance
}

// approvalLabel says when an approval covers more than this one trade, so the
// wallet prompt and the step list agree about what is being signed.
func approvalLabel(format string, token *chain.Token, grant, amountIn *big.Int) string {
	label := fmt.Sprintf(format, token.Symbol)
	if grant.Cmp(amountIn) > 0 {
		return label + fmt.Sprintf(" (up to %s %s, once)", formatUnits(grant, token.Decimals), token.Symbol)
	}
	return label
}

func (s *Server) buildSwapV4(w http.ResponseWriter, r *http.Request, req txBuildRequest, ch chain.Chain, p chain.Pool,
	amountIn, minOut *big.Int) {
	if !s.v4Ready(w, ch, p) {
		return
	}
	inToken, outToken, _, _ := tradeSides(chain.Orient(ch, p.Token0, p.Token1), req.Side, nil, nil)
	native, ok := payNativeFor(w, ch, req.Side, req.PayNative, inToken)
	if !ok {
		return
	}
	ctx := r.Context()
	key, err := s.v4Key(ctx, ch, p)
	if err != nil {
		s.log.Warn("v4 pool key unavailable", "pool", p.Address, "error", err)
		writeError(w, http.StatusBadGateway, "pool_key_unavailable",
			"this pool's parameters could not be read from the chain; try again in a moment")
		return
	}
	zeroForOne := sameAddress(inToken, key.Currency0)
	if !zeroForOne && !sameAddress(inToken, key.Currency1) {
		writeError(w, http.StatusConflict, "pool_mismatch", "this pool's stored tokens do not match its on-chain key")
		return
	}

	deadline := req.DeadlineSecs
	if deadline <= 0 || deadline > 3600 {
		deadline = 300
	}
	expires := time.Now().Add(time.Duration(deadline) * time.Second)

	swap := uniswapv4.Swap{
		Key: key, ZeroForOne: zeroForOne, AmountIn: amountIn, MinOut: minOut,
		Deadline:   big.NewInt(expires.Unix()),
		ExtraParam: ch.UniswapV4.ExtraSwapParam,
		WrapNative: native,
	}
	data, err := uniswapv4.EncodeExecute(swap)
	if err != nil {
		writeError(w, http.StatusBadRequest, "encode_failed", err.Error())
		return
	}

	evm := s.pool.EVM(ch.ID)
	router := common.HexToAddress(ch.UniswapV4.UniversalRouter)
	from := common.HexToAddress(req.From)
	steps := []UnsignedTx{}

	// Paying a token: the token must let Permit2 move it, and Permit2 must let
	// the router pull it. Each shortfall becomes a step the wallet signs first,
	// sized to the wallet's balance so the next trade in the token needs none.
	if !native {
		approvals, err := s.approvalSteps(ctx, ch, p, from, inToken, amountIn, expires)
		if err != nil {
			// A shortfall that could not be read is caught by the simulation below.
			s.log.Debug("v4 build could not read every allowance", "pool", p.Address, "error", err)
		}
		steps = append(steps, approvals...)
	}

	value := swap.PayableValue()
	swapTx := UnsignedTx{
		ChainID: ch.ID, To: router.Hex(), Data: hexutil.Encode(data), Value: value.String(),
		Kind: "swap", Label: fmt.Sprintf("Swap %s → %s on Uniswap v4", inToken.Symbol, outToken.Symbol),
	}
	payingSymbol := inToken.Symbol
	if native {
		payingSymbol = ch.NativeSymbol
		swapTx.Label = fmt.Sprintf("Swap %s → %s on Uniswap v4", ch.NativeSymbol, outToken.Symbol)
	}

	// Simulate from the user's own account when nothing is pending; a swap
	// simulated before its approvals are mined reverts for the wrong reason.
	if len(steps) == 0 {
		if _, err := evm.Simulate(ctx, from, router, data, value, nil); err != nil {
			switch {
			case isInsufficientFunds(err):
				writeError(w, http.StatusUnprocessableEntity, "insufficient_funds",
					fmt.Sprintf("this wallet does not hold enough %s for the trade", payingSymbol))
				return
			case rpcpool.IsExecutionReverted(err):
				reason := rpcpool.DecodeRevert(err)
				if reason == "" {
					reason = "the swap reverts in simulation: the price moved past your minimum, the balance is short, or the token or its hook blocks the transfer"
				}
				writeError(w, http.StatusUnprocessableEntity, "simulation_reverted", reason)
				return
			default:
				swapTx.Label += " (not simulated)"
			}
		}
		if gas, err := evm.EstimateGas(ctx, from, router, data, value); err == nil {
			swapTx.Gas = gas + gas/5
		}
	}
	steps = append(steps, swapTx)

	writeJSON(w, http.StatusOK, map[string]any{
		"steps":     steps,
		"expiresAt": expires,
		"note":      "sign these in order with your own wallet; this service holds no keys and cannot sign",
	})
}

// nativePriceUSD is the chain's native-currency price, or failing that the
// price of the same asset read on another chain: Robinhood and Arbitrum pay
// gas in ETH but have no reference pool of their own.
func (s *Server) nativePriceUSD(ctx context.Context, ch chain.Chain) (float64, bool) {
	if ch.NativeIsStable {
		return 1, true
	}
	if usd, ok, _ := s.store.NativePrice(ctx, ch.ID); ok && usd > 0 {
		return usd, true
	}
	for _, peer := range s.reg.All() {
		if peer.ID == ch.ID || peer.NativeSymbol == "" || peer.NativeSymbol != ch.NativeSymbol {
			continue
		}
		if usd, ok, _ := s.store.NativePrice(ctx, peer.ID); ok && usd > 0 {
			return usd, true
		}
	}
	return 0, false
}

func isInsufficientFunds(err error) bool {
	msg := strings.ToLower(err.Error())
	// Geth says "insufficient funds"; Base's reth nodes say "EVM error: OutOfFunds".
	return strings.Contains(msg, "insufficient funds") || strings.Contains(msg, "insufficient balance") ||
		strings.Contains(msg, "outoffunds")
}

func shortAddr(a string) string {
	if len(a) < 12 {
		return a
	}
	return a[:6] + "…" + a[len(a)-4:]
}
