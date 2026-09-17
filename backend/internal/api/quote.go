package api

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/rpcpool"
)

// Quoting and transaction building.
//
// The service never signs and never holds keys. Everything here produces an
// unsigned payload that the user's wallet reviews and signs, and a quote always
// carries the minimum-received figure that gets enforced on-chain, not merely
// displayed.

const routerV2ABI = `[
  {"name":"swapExactTokensForTokens","inputs":[
    {"name":"amountIn","type":"uint256"},{"name":"amountOutMin","type":"uint256"},
    {"name":"path","type":"address[]"},{"name":"to","type":"address"},
    {"name":"deadline","type":"uint256"}],
   "outputs":[{"type":"uint256[]"}],"stateMutability":"nonpayable","type":"function"},
  {"name":"swapExactTokensForTokensSupportingFeeOnTransferTokens","inputs":[
    {"name":"amountIn","type":"uint256"},{"name":"amountOutMin","type":"uint256"},
    {"name":"path","type":"address[]"},{"name":"to","type":"address"},
    {"name":"deadline","type":"uint256"}],
   "outputs":[],"stateMutability":"nonpayable","type":"function"},
  {"name":"swapExactETHForTokensSupportingFeeOnTransferTokens","inputs":[
    {"name":"amountOutMin","type":"uint256"},{"name":"path","type":"address[]"},
    {"name":"to","type":"address"},{"name":"deadline","type":"uint256"}],
   "outputs":[],"stateMutability":"payable","type":"function"}
]`

const erc20MiniABI = `[
  {"name":"approve","inputs":[{"type":"address"},{"type":"uint256"}],"outputs":[{"type":"bool"}],"stateMutability":"nonpayable","type":"function"},
  {"name":"allowance","inputs":[{"type":"address"},{"type":"address"}],"outputs":[{"type":"uint256"}],"stateMutability":"view","type":"function"},
  {"name":"getReserves","inputs":[],"outputs":[{"type":"uint112"},{"type":"uint112"},{"type":"uint32"}],"stateMutability":"view","type":"function"}
]`

var (
	routerV2  = mustABI(routerV2ABI)
	erc20Mini = mustABI(erc20MiniABI)
)

func mustABI(s string) abi.ABI {
	a, err := abi.JSON(strings.NewReader(s))
	if err != nil {
		panic("api: bad built-in ABI: " + err.Error())
	}
	return a
}

// Leg is one step in the plan the UI renders verbatim.
type Leg struct {
	Step  string `json:"step"`
	Label string `json:"label"`
	Value string `json:"value"`
}

// QuoteResponse is the shape every quote endpoint returns.
type QuoteResponse struct {
	Legs           []Leg     `json:"legs"`
	AmountIn       string    `json:"amountIn"`
	AmountOut      string    `json:"amountOut"`
	MinReceived    string    `json:"minReceived"`
	PriceImpactBps int       `json:"priceImpactBps"`
	Rate           string    `json:"rate"`
	EstGas         uint64    `json:"estGas"`
	GasPriceWei    string    `json:"gasPriceWei"`
	EstGasNative   string    `json:"estGasNative"`
	EstGasUSD      *float64  `json:"estGasUsd"`
	ExpiresAt      time.Time `json:"expiresAt"`
	Gated          bool      `json:"gated"`
	GateReason     string    `json:"gateReason,omitempty"`
}

type swapQuoteRequest struct {
	ChainID       chain.ID `json:"chainId"`
	Pool          string   `json:"pool"`
	Side          string   `json:"side"`     // "buy" | "sell"
	AmountIn      string   `json:"amountIn"` // decimal string in base units
	SlippageBps   int      `json:"slippageBps"`
	RiskThreshold int      `json:"riskThreshold,omitempty"`
	// PayNative buys with the native currency on a pair priced in its wrapped
	// form: ETH instead of WETH. The router wraps it in the same transaction.
	PayNative bool `json:"payNative,omitempty"`
}

// payNativeFor validates a payNative request against the trade. It is a buy
// paid in the chain's wrapped native token, or it is refused.
func payNativeFor(w http.ResponseWriter, ch chain.Chain, side string, payNative bool, inToken *chain.Token) (bool, bool) {
	if !payNative {
		return false, true
	}
	if side != "buy" {
		writeError(w, http.StatusBadRequest, "pay_native_unsupported", "payNative applies to buys; a sell pays out the pair's own quote token")
		return false, false
	}
	if inToken == nil || ch.WrappedNative == "" || !strings.EqualFold(inToken.Address, ch.WrappedNative) {
		writeError(w, http.StatusBadRequest, "pay_native_unsupported",
			fmt.Sprintf("this pair is not priced in wrapped %s, so it cannot be paid in %s", ch.NativeSymbol, ch.NativeSymbol))
		return false, false
	}
	return true, true
}

// nativeLegs puts the wrap in front of a quote's route when paying natively.
func nativeLegs(ch chain.Chain, amountIn *big.Int, inToken, outToken *chain.Token, venue string) []Leg {
	return []Leg{
		{Step: "1", Label: fmt.Sprintf("Wrap %s → %s (inside the swap)", ch.NativeSymbol, inToken.Symbol), Value: formatUnits(amountIn, 18) + " " + ch.NativeSymbol},
		{Step: "2", Label: fmt.Sprintf("Swap %s → %s%s", inToken.Symbol, outToken.Symbol, venue), Value: formatUnits(amountIn, inToken.Decimals) + " " + inToken.Symbol},
	}
}

func (s *Server) handleQuoteSwap(w http.ResponseWriter, r *http.Request) {
	var req swapQuoteRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.Side != "buy" && req.Side != "sell" {
		writeError(w, http.StatusBadRequest, "bad_side", `side must be "buy" or "sell"`)
		return
	}
	amountIn, ok := new(big.Int).SetString(strings.TrimSpace(req.AmountIn), 10)
	if !ok || amountIn.Sign() <= 0 {
		writeError(w, http.StatusBadRequest, "bad_amount",
			"amountIn must be a positive decimal string in the token's base units")
		return
	}
	slippage := req.SlippageBps
	if slippage <= 0 || slippage > 5000 {
		slippage = 100 // 1.0%, the UI's default
	}

	ch, ok := s.reg.Get(req.ChainID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown_chain", "no such chain")
		return
	}
	p, err := s.store.GetPool(r.Context(), req.ChainID, req.Pool)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// The risk gate applies to buys only. A user must always be able to exit a
	// position, whatever the score says about the pool.
	gated, reason := false, ""
	if req.Side == "buy" && p.Risk != nil && s.scorer.Gated(p.Risk.Score, req.RiskThreshold) {
		gated = true
		reason = p.Risk.Verdict
	}

	if isV4Pool(p) {
		s.quoteSwapV4(w, r, req, ch, p, amountIn, slippage, gated, reason)
		return
	}
	if isV3Pool(ch, p) {
		s.quoteSwapV3(w, r, req, ch, p, amountIn, slippage, gated, reason)
		return
	}
	if p.Model != chain.ModelV2 {
		// Stableswap quoting needs curve math that is not implemented, and a
		// constant-product estimate against such a pool would be wrong by an
		// amount that depends on the curve. Refusing is the only honest answer.
		writeError(w, http.StatusNotImplemented, "model_unsupported",
			fmt.Sprintf("quoting is implemented for constant-product, Uniswap v3 and Uniswap v4 pools; this pool is %s %q", p.Dex, p.Model))
		return
	}
	if p.Token0 == nil || p.Token1 == nil || p.Token0.Symbol == "" || p.Token1.Symbol == "" {
		writeError(w, http.StatusConflict, "not_enriched",
			"this pool's tokens are not resolved yet; try again in a moment")
		return
	}

	r0, r1, err := s.pairReserves(r.Context(), req.ChainID, p.Address)
	if err != nil || r0 == nil || r1 == nil || r0.Sign() == 0 || r1.Sign() == 0 {
		writeError(w, http.StatusConflict, "no_reserves",
			"this pool's reserves are not known yet; try again in a moment")
		return
	}

	inToken, outToken, reserveIn, reserveOut := tradeSides(chain.Orient(ch, p.Token0, p.Token1), req.Side, r0, r1)
	native, ok := payNativeFor(w, ch, req.Side, req.PayNative, inToken)
	if !ok {
		return
	}

	feeBps := 30
	if p.FeeTierBps != nil {
		feeBps = *p.FeeTierBps
	}

	amountOut := amountOutV2(amountIn, reserveIn, reserveOut, feeBps)
	if amountOut.Sign() <= 0 {
		writeError(w, http.StatusBadRequest, "no_output",
			"that input is too small to produce any output at this pool's liquidity")
		return
	}

	minReceived := new(big.Int).Div(
		new(big.Int).Mul(amountOut, big.NewInt(int64(10000-slippage))),
		big.NewInt(10000))

	resp := QuoteResponse{
		Legs: []Leg{{
			Step:  "1",
			Label: fmt.Sprintf("Swap %s → %s", inToken.Symbol, outToken.Symbol),
			Value: formatUnits(amountIn, inToken.Decimals) + " " + inToken.Symbol,
		}},
		AmountIn:       amountIn.String(),
		AmountOut:      amountOut.String(),
		MinReceived:    minReceived.String(),
		PriceImpactBps: priceImpactBps(amountIn, amountOut, reserveIn, reserveOut),
		Rate: fmt.Sprintf("1 %s = %s %s", outToken.Symbol,
			spotRate(reserveIn, reserveOut, inToken.Decimals, outToken.Decimals), inToken.Symbol),
		ExpiresAt:  time.Now().Add(30 * time.Second),
		Gated:      gated,
		GateReason: reason,
	}

	// Gas is priced from the chain, and only converted to USD when the native
	// price has actually been established. Otherwise the field stays null and
	// the UI shows an em dash rather than a made-up dollar figure.
	var swapGasEstimate int64 = 180_000
	if native {
		resp.Legs = nativeLegs(ch, amountIn, inToken, outToken, "")
		swapGasEstimate += 25_000 // the router's WETH deposit
	}
	resp.EstGas = uint64(swapGasEstimate)
	evm := s.pool.EVM(req.ChainID)
	if gasPrice, err := evm.GasPrice(r.Context()); err == nil && gasPrice != nil {
		cost := new(big.Int).Mul(gasPrice, big.NewInt(swapGasEstimate))
		resp.GasPriceWei = gasPrice.String()
		resp.EstGasNative = formatUnits(cost, 18)
		if native, ok := s.nativePriceUSD(r.Context(), ch); ok {
			usd := parseFloat(resp.EstGasNative) * native
			resp.EstGasUSD = &usd
		}
	}
	_ = ch

	writeJSON(w, http.StatusOK, resp)
}

// handleQuoteZap and handleQuoteExit are declared but not implemented.
//
// Both are single-transaction, multi-leg operations — swap then deposit, or
// burn then claim then sell — and "reverts entirely if any leg fails" is a
// promise only an on-chain router can keep. There is no such contract deployed
// for this service, and assembling the legs client-side would break exactly the
// atomicity the UI tells the user it has. Returning 501 is the honest answer
// until the router exists.
func (s *Server) handleQuoteZap(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "router_not_deployed",
		"zap quoting needs the atomic router contract, which is not deployed yet; "+
			"the UI's single-transaction guarantee cannot be honoured without it")
}

func (s *Server) handleQuoteExit(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "router_not_deployed",
		"remove-and-sell needs the atomic router contract, which is not deployed yet; "+
			"the UI's single-transaction guarantee cannot be honoured without it")
}

type txBuildRequest struct {
	ChainID       chain.ID `json:"chainId"`
	Pool          string   `json:"pool"`
	Side          string   `json:"side"`
	AmountIn      string   `json:"amountIn"`
	MinReceived   string   `json:"minReceived"`
	From          string   `json:"from"`
	DeadlineSecs  int      `json:"deadlineSecs,omitempty"`
	RiskThreshold int      `json:"riskThreshold,omitempty"`
	// PayNative: see swapQuoteRequest.
	PayNative bool `json:"payNative,omitempty"`
}

// UnsignedTx is what the wallet signs. There is no signature field and never
// will be: this service does not hold keys.
type UnsignedTx struct {
	ChainID chain.ID `json:"chainId"`
	To      string   `json:"to"`
	Data    string   `json:"data"`
	Value   string   `json:"value"`
	Gas     uint64   `json:"gas,omitempty"`
	Kind    string   `json:"kind"`
	Label   string   `json:"label"`
}

func (s *Server) handleTxBuild(w http.ResponseWriter, r *http.Request) {
	var req txBuildRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !common.IsHexAddress(req.From) {
		writeError(w, http.StatusBadRequest, "bad_from", "from must be a hex address")
		return
	}
	amountIn, ok := new(big.Int).SetString(strings.TrimSpace(req.AmountIn), 10)
	if !ok || amountIn.Sign() <= 0 {
		writeError(w, http.StatusBadRequest, "bad_amount", "amountIn must be a positive decimal string")
		return
	}
	minOut, ok := new(big.Int).SetString(strings.TrimSpace(req.MinReceived), 10)
	if !ok || minOut.Sign() < 0 {
		writeError(w, http.StatusBadRequest, "bad_min_received",
			"minReceived must be a non-negative decimal string; it is enforced on-chain")
		return
	}

	if req.Side != "buy" && req.Side != "sell" {
		writeError(w, http.StatusBadRequest, "bad_side", `side must be "buy" or "sell"`)
		return
	}

	ch, ok := s.reg.Get(req.ChainID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown_chain", "no such chain")
		return
	}

	p, err := s.store.GetPool(r.Context(), req.ChainID, req.Pool)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	// The gate, again, and only for buys.
	if req.Side == "buy" && p.Risk != nil && s.scorer.Gated(p.Risk.Score, req.RiskThreshold) {
		writeError(w, http.StatusForbidden, "risk_gated", p.Risk.Verdict)
		return
	}

	if isV4Pool(p) {
		s.buildSwapV4(w, r, req, ch, p, amountIn, minOut)
		return
	}
	if isV3Pool(ch, p) {
		s.buildSwapV3(w, r, req, ch, p, amountIn, minOut)
		return
	}
	if ch.RouterV2 == "" {
		writeError(w, http.StatusNotImplemented, "no_router",
			"no v2 router is configured for this chain")
		return
	}
	if p.Model != chain.ModelV2 || p.Token0 == nil || p.Token1 == nil {
		writeError(w, http.StatusNotImplemented, "model_unsupported",
			"transaction building is implemented for constant-product, Uniswap v3 and Uniswap v4 pools only")
		return
	}

	inToken, outToken, _, _ := tradeSides(chain.Orient(ch, p.Token0, p.Token1), req.Side, nil, nil)
	native, ok := payNativeFor(w, ch, req.Side, req.PayNative, inToken)
	if !ok {
		return
	}

	deadline := req.DeadlineSecs
	if deadline <= 0 || deadline > 3600 {
		deadline = 300
	}
	deadlineTS := big.NewInt(time.Now().Add(time.Duration(deadline) * time.Second).Unix())

	router := common.HexToAddress(ch.RouterV2)
	from := common.HexToAddress(req.From)
	path := []common.Address{
		common.HexToAddress(inToken.Address),
		common.HexToAddress(outToken.Address),
	}

	// Fee-on-transfer tokens break the standard swap function's accounting, and
	// the supporting variant is correct for both. Using it unconditionally
	// costs nothing and avoids a class of confusing reverts.
	var data []byte
	value := big.NewInt(0)
	if native {
		// The router wraps the value to WETH and swaps along the same path.
		data, err = routerV2.Pack("swapExactETHForTokensSupportingFeeOnTransferTokens", minOut, path, from, deadlineTS)
		value = amountIn
	} else {
		data, err = routerV2.Pack("swapExactTokensForTokensSupportingFeeOnTransferTokens",
			amountIn, minOut, path, from, deadlineTS)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode_failed", "could not encode the swap")
		return
	}

	evm := s.pool.EVM(req.ChainID)
	steps := []UnsignedTx{}

	// An allowance check up front turns "your wallet said the transaction will
	// fail" into an explicit approval step the user understands. The approval
	// covers the wallet's balance, so trading the same token again needs none.
	if !native {
		approvals, err := s.approvalSteps(r.Context(), ch, p, from, inToken, amountIn, time.Now().Add(time.Duration(deadline)*time.Second))
		if err != nil {
			s.log.Debug("v2 build could not read the allowance", "pool", p.Address, "error", err)
		}
		steps = append(steps, approvals...)
	}

	swapTx := UnsignedTx{
		ChainID: req.ChainID,
		To:      router.Hex(),
		Data:    hexutil.Encode(data),
		Value:   value.String(),
		Kind:    "swap",
		Label:   fmt.Sprintf("Swap %s → %s", inToken.Symbol, outToken.Symbol),
	}
	if native {
		swapTx.Label = fmt.Sprintf("Swap %s → %s", ch.NativeSymbol, outToken.Symbol)
	}

	// Simulate before handing anything over — but only when there is no pending
	// approval, since a simulation without allowance reverts for a reason that
	// has nothing to do with the trade.
	if len(steps) == 0 {
		if _, err := evm.Simulate(r.Context(), from, router, data, value, nil); err != nil {
			if isInsufficientFunds(err) {
				symbol := inToken.Symbol
				if native {
					symbol = ch.NativeSymbol
				}
				writeError(w, http.StatusUnprocessableEntity, "insufficient_funds",
					fmt.Sprintf("this wallet does not hold enough %s for the trade", symbol))
				return
			}
			if rpcpool.IsExecutionReverted(err) {
				reason := rpcpool.DecodeRevert(err)
				if reason == "" {
					reason = "the swap reverts in simulation"
				}
				writeError(w, http.StatusUnprocessableEntity, "simulation_reverted", reason)
				return
			}
			// Could not simulate (provider limitation, transient failure). Say
			// so on the response rather than implying the trade was checked.
			swapTx.Label += " (not simulated)"
		}
		if gas, err := evm.EstimateGas(r.Context(), from, router, data, value); err == nil {
			swapTx.Gas = gas + gas/5 // 20% headroom
		}
	}

	steps = append(steps, swapTx)

	writeJSON(w, http.StatusOK, map[string]any{
		"steps":     steps,
		"expiresAt": time.Now().Add(time.Duration(deadline) * time.Second),
		"note":      "sign these in order with your own wallet; this service holds no keys and cannot sign",
	})
}

type txSendRequest struct {
	ChainID chain.ID `json:"chainId"`
	Raw     string   `json:"raw"`
}

// handleTxSend relays an already-signed transaction. It exists only so a client
// on a restricted network can broadcast through us; the service cannot produce
// a signature of its own and does not inspect the payload beyond its encoding.
func (s *Server) handleTxSend(w http.ResponseWriter, r *http.Request) {
	var req txSendRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	raw, err := hexutil.Decode(req.Raw)
	if err != nil || len(raw) == 0 {
		writeError(w, http.StatusBadRequest, "bad_raw", "raw must be a 0x-prefixed signed transaction")
		return
	}
	if _, ok := s.reg.Get(req.ChainID); !ok {
		writeError(w, http.StatusBadRequest, "unknown_chain", "no such chain")
		return
	}

	hash, err := s.pool.EVM(req.ChainID).SendRawTransaction(r.Context(), raw)
	if err != nil {
		writeError(w, http.StatusBadGateway, "broadcast_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"txHash": hash.Hex()})
}

// pairReserves reads a v2-style pair's reserves from the chain, as of now.
//
// A quote needs the reserves the swap will actually meet. They used to come
// from a table the enricher filled; the market tracker now keeps pool state in
// memory and nothing writes that table, so every v2 quote failed with
// "reserves are not known yet". One getReserves call is both current and cheap.
func (s *Server) pairReserves(ctx context.Context, chainID chain.ID, pair string) (*big.Int, *big.Int, error) {
	data, err := erc20Mini.Pack("getReserves")
	if err != nil {
		return nil, nil, err
	}
	out, err := s.pool.EVM(chainID).CallContract(ctx, common.HexToAddress(pair), data, nil, nil)
	if err != nil {
		return nil, nil, err
	}
	vals, err := erc20Mini.Unpack("getReserves", out)
	if err != nil || len(vals) < 2 {
		return nil, nil, fmt.Errorf("api: decode reserves of %s: %v", pair, err)
	}
	r0, ok0 := vals[0].(*big.Int)
	r1, ok1 := vals[1].(*big.Int)
	if !ok0 || !ok1 {
		return nil, nil, fmt.Errorf("api: reserves of %s are not numbers", pair)
	}
	return r0, r1, nil
}

// tradeSides maps a buy or sell of a pool's traded token onto what goes in and
// what comes out, and onto the pool's reserves, which are reported by slot.
//
// A buy pays the quote and receives the base; a sell is the reverse. Deciding
// this from slot order instead — "token0 is the one being bought" — builds the
// opposite trade for every pool whose quote asset sorts first, which for WETH
// pools is a large share of them. r0 and r1 may be nil when only the tokens
// are needed.
func tradeSides(o chain.Orientation, side string, r0, r1 *big.Int) (in, out *chain.Token, reserveIn, reserveOut *big.Int) {
	reserveBase, reserveQuote := r0, r1
	if !o.BaseIsToken0 {
		reserveBase, reserveQuote = r1, r0
	}
	if side == "sell" {
		return o.Base, o.Quote, reserveBase, reserveQuote
	}
	return o.Quote, o.Base, reserveQuote, reserveBase
}

// amountOutV2 is the constant-product formula with the pool's fee.
func amountOutV2(amountIn, reserveIn, reserveOut *big.Int, feeBps int) *big.Int {
	if feeBps < 0 || feeBps > 10000 {
		feeBps = 30
	}
	amountInWithFee := new(big.Int).Mul(amountIn, big.NewInt(int64(10000-feeBps)))
	numerator := new(big.Int).Mul(amountInWithFee, reserveOut)
	denominator := new(big.Int).Add(
		new(big.Int).Mul(reserveIn, big.NewInt(10000)),
		amountInWithFee)
	if denominator.Sign() == 0 {
		return big.NewInt(0)
	}
	return new(big.Int).Div(numerator, denominator)
}

// priceImpactBps compares the executed rate with the spot rate.
func priceImpactBps(amountIn, amountOut, reserveIn, reserveOut *big.Int) int {
	if amountIn.Sign() == 0 || reserveIn.Sign() == 0 {
		return 0
	}
	spot := new(big.Float).Quo(new(big.Float).SetInt(reserveOut), new(big.Float).SetInt(reserveIn))
	exec := new(big.Float).Quo(new(big.Float).SetInt(amountOut), new(big.Float).SetInt(amountIn))
	if spot.Sign() == 0 {
		return 0
	}
	diff := new(big.Float).Sub(spot, exec)
	ratio := new(big.Float).Quo(diff, spot)
	f, _ := ratio.Float64()
	if f < 0 {
		f = 0
	}
	return int(f * 10000)
}

func spotRate(reserveIn, reserveOut *big.Int, decIn, decOut int) string {
	if reserveOut.Sign() == 0 {
		return "0"
	}
	in := new(big.Float).Quo(new(big.Float).SetInt(reserveIn), pow10(decIn))
	out := new(big.Float).Quo(new(big.Float).SetInt(reserveOut), pow10(decOut))
	if out.Sign() == 0 {
		return "0"
	}
	rate, _ := new(big.Float).Quo(in, out).Float64()
	return trimFloat(rate)
}

func pow10(n int) *big.Float {
	return new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil))
}

// formatUnits renders a base-unit amount with its decimals, as a decimal
// string. Amounts stay big.Int everywhere they matter; this is display only.
func formatUnits(v *big.Int, decimals int) string {
	if v == nil {
		return "0"
	}
	f := new(big.Float).Quo(new(big.Float).SetInt(v), pow10(decimals))
	out, _ := f.Float64()
	return trimFloat(out)
}

// trimFloat renders a float for display with enough decimals to show at least
// four significant digits: new tokens routinely trade below 0.00000001 of their
// quote, and a fixed eight decimals rounded their rate to nothing.
func trimFloat(f float64) string {
	decimals := 8
	if a := math.Abs(f); a > 0 && a < 1e-4 {
		decimals = min(int(math.Ceil(-math.Log10(a)))+4, 24)
	}
	s := strconv.FormatFloat(f, 'f', decimals, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

func parseFloat(s string) float64 {
	var f float64
	_, _ = fmt.Sscanf(s, "%g", &f)
	return f
}
