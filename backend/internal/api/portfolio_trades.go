package api

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/go-chi/chi/v5"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/rpcpool"
	"github.com/earlypair/backend/internal/store"
	"github.com/earlypair/backend/internal/uniswapv3"
	"github.com/earlypair/backend/internal/uniswapv4"
)

// Portfolio holdings are built from trades made through EarlyPair, each one
// verified against its mined receipt before it is recorded.
//
// That is a deliberate boundary. Every acquisition a wallet ever made — other
// apps, transfers, airdrops — is not something this service can see, and a cost
// basis that silently omitted them would report a wrong P&L. So a holding's
// balance is always read live from the chain, its cost basis comes only from
// verified trades, and when the balance holds more than those trades explain the
// P&L is left empty rather than computed from part of the picture.

type recordTradeRequest struct {
	ChainID chain.ID `json:"chainId"`
	Pool    string   `json:"pool"`
	TxHash  string   `json:"txHash"`
}

// handleRecordTrade verifies a swap from its receipt and records it. It needs
// no session: the receipt itself proves who traded, and it only ever records
// what the chain shows.
func (s *Server) handleRecordTrade(w http.ResponseWriter, r *http.Request) {
	var req recordTradeRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if len(strings.TrimPrefix(req.TxHash, "0x")) != 64 {
		writeError(w, http.StatusBadRequest, "bad_tx_hash", "txHash must be a 32-byte hex hash")
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
	if p.Token0 == nil || p.Token1 == nil {
		writeError(w, http.StatusConflict, "not_enriched", "this pool's tokens are not resolved yet")
		return
	}

	mined, err := s.pool.EVM(ch.ID).MinedTransaction(r.Context(), common.HexToHash(req.TxHash))
	if errors.Is(err, rpcpool.ErrNotMined) {
		// The endpoint asked may not have seen the block yet; the client retries.
		writeError(w, http.StatusNotFound, "not_mined", "that transaction has no receipt yet; try again shortly")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "receipt_unavailable", "the transaction could not be read from the chain")
		return
	}
	trade, err := verifyTrade(ch, p, common.HexToHash(req.TxHash), mined)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "not_a_trade", err.Error())
		return
	}
	if usd, ok := s.quoteValueUSD(r.Context(), ch, trade.QuoteToken, trade.QuoteAmount, quoteDecimals(p, trade.QuoteToken)); ok {
		trade.QuoteUSD = &usd
	}

	created, err := s.store.InsertTrade(r.Context(), trade)
	if err != nil {
		s.log.Error("record trade failed", "tx", req.TxHash, "error", err)
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"recorded":    created,
		"wallet":      trade.Wallet,
		"side":        trade.Side,
		"token":       trade.Token,
		"tokenAmount": trade.TokenAmount,
		"quoteAmount": trade.QuoteAmount,
		"quoteUsd":    trade.QuoteUSD,
	})
}

// verifyTrade reads a trade out of a receipt: it must have succeeded, gone
// through one of this chain's configured routers, and swapped in the pool. The
// pool's own Swap event gives the amounts; tokens that reached (or left) the
// wallet by Transfer override the traded token's amount, so a token that taxes
// transfers records what the wallet actually received.
func verifyTrade(ch chain.Chain, p chain.Pool, hash common.Hash, tx rpcpool.MinedTx) (store.Trade, error) {
	if !tx.Success {
		return store.Trade{}, errors.New("that transaction reverted")
	}
	if tx.To == nil || !isTradeRouter(ch, *tx.To) {
		return store.Trade{}, errors.New("that transaction did not go through a router this service builds trades for")
	}

	var amount0, amount1 *big.Int
	for _, l := range tx.Logs {
		sw, ok := poolSwap(ch, p, l)
		if !ok {
			continue
		}
		if amount0 == nil {
			amount0, amount1 = new(big.Int), new(big.Int)
		}
		amount0.Add(amount0, sw.Amount0)
		amount1.Add(amount1, sw.Amount1)
	}
	if amount0 == nil {
		return store.Trade{}, errors.New("that transaction did not swap in this pool")
	}

	o := chain.Orient(ch, p.Token0, p.Token1)
	// Pool-side amounts: positive entered the pool, negative left it.
	baseDelta, quoteDelta := amount0, amount1
	if !o.BaseIsToken0 {
		baseDelta, quoteDelta = amount1, amount0
	}
	side := "buy"
	if baseDelta.Sign() > 0 {
		side = "sell"
	}
	if baseDelta.Sign() == 0 || quoteDelta.Sign() == 0 {
		return store.Trade{}, errors.New("that swap moved nothing")
	}

	tokenAmount := new(big.Int).Abs(baseDelta)
	if moved := transferred(tx.Logs, common.HexToAddress(o.Base.Address), tx.From, side == "buy"); moved.Sign() > 0 {
		tokenAmount = moved
	}
	return store.Trade{
		ChainID: ch.ID, TxHash: hash.Hex(), Wallet: tx.From.Hex(), Pool: p.Address,
		Token: o.Base.Address, Side: side, TokenAmount: tokenAmount.String(),
		QuoteToken: o.Quote.Address, QuoteAmount: new(big.Int).Abs(quoteDelta).String(),
		BlockNumber: tx.BlockNumber,
	}, nil
}

// isTradeRouter lists the routers this service builds swaps through. A trade is
// recorded only when its transaction went through one of them: the receipt then
// proves both who traded and that this service built it. Every router the build
// side can emit belongs here — v3's SwapRouter02 was missing at first, and v3
// buys silently never reached the portfolio.
func isTradeRouter(ch chain.Chain, to common.Address) bool {
	if ch.RouterV2 != "" && common.HexToAddress(ch.RouterV2) == to {
		return true
	}
	if ch.UniswapV3 != nil && ch.UniswapV3.SwapRouter != "" && common.HexToAddress(ch.UniswapV3.SwapRouter) == to {
		return true
	}
	return ch.UniswapV4 != nil && ch.UniswapV4.UniversalRouter != "" && common.HexToAddress(ch.UniswapV4.UniversalRouter) == to
}

// poolSwap decodes a log when it is this pool's Swap event.
func poolSwap(ch chain.Chain, p chain.Pool, l types.Log) (chain.Swap, bool) {
	if len(l.Topics) == 0 {
		return chain.Swap{}, false
	}
	if isV4Pool(p) {
		if l.Topics[0] != chain.TopicSwapV4 || len(l.Topics) < 2 || l.Topics[1] != common.HexToHash(p.Address) {
			return chain.Swap{}, false
		}
		for _, f := range ch.Factories {
			if f.Event == chain.EventInitializeV4 && common.HexToAddress(f.Address) == l.Address {
				sw, err := chain.DecodeSwapV4(l)
				return sw, err == nil
			}
		}
		return chain.Swap{}, false
	}
	if l.Address != common.HexToAddress(p.Address) {
		return chain.Swap{}, false
	}
	switch l.Topics[0] {
	case chain.TopicSwapV2, chain.TopicSwapSolidly, chain.TopicSwapV3, chain.TopicSwapPancakeV3:
		sw, err := chain.DecodeSwap(l)
		return sw, err == nil
	}
	return chain.Swap{}, false
}

// transferred sums a token's Transfer events to (or from) an address.
func transferred(logs []types.Log, token, wallet common.Address, incoming bool) *big.Int {
	total := new(big.Int)
	for _, l := range logs {
		if l.Address != token || len(l.Topics) < 3 || l.Topics[0] != chain.TopicTransfer || len(l.Data) < 32 {
			continue
		}
		party := l.Topics[1]
		if incoming {
			party = l.Topics[2]
		}
		if common.BytesToAddress(party.Bytes()) == wallet {
			total.Add(total, new(big.Int).SetBytes(l.Data[:32]))
		}
	}
	return total
}

func quoteDecimals(p chain.Pool, quote string) int {
	for _, t := range []*chain.Token{p.Token0, p.Token1} {
		if t != nil && strings.EqualFold(t.Address, quote) {
			return t.Decimals
		}
	}
	return 18
}

// quoteUSD is a quote asset's dollar price when it is established: a stable is
// a dollar, the native currency and its wrapper the native price.
func (s *Server) quoteUSD(ctx context.Context, ch chain.Chain, token string) (float64, bool) {
	for _, st := range ch.Stables {
		if strings.EqualFold(st, token) {
			return 1, true
		}
	}
	if chain.IsNative(token) || strings.EqualFold(ch.WrappedNative, token) {
		return s.nativePriceUSD(ctx, ch)
	}
	return 0, false
}

func (s *Server) quoteValueUSD(ctx context.Context, ch chain.Chain, token, amount string, decimals int) (float64, bool) {
	usd, ok := s.quoteUSD(ctx, ch, token)
	v, okAmount := new(big.Int).SetString(amount, 10)
	if !ok || !okAmount {
		return 0, false
	}
	return parseFloat(formatUnits(v, decimals)) * usd, true
}

// ------------------------------------------------------------------ holdings

// Holding is one token line on the portfolio page.
type Holding struct {
	ChainID     chain.ID           `json:"chainId"`
	Pool        string             `json:"pool"`
	Token       chain.Token        `json:"token"`
	Amount      string             `json:"amount"`
	AvgEntryUSD *float64           `json:"avgEntryUsd"`
	PriceUSD    *float64           `json:"priceUsd"`
	ValueUSD    *float64           `json:"valueUsd"`
	PnlPct      *float64           `json:"pnlPct"`
	PnlUSD      *float64           `json:"pnlUsd"`
	Risk        *chain.RiskSummary `json:"risk"`
	// SellReady says whether selling the whole balance needs no approval first,
	// so the page can offer to approve ahead and make the sell one transaction.
	// Null when the allowances could not be read.
	SellReady *bool `json:"sellReady"`
	// Note says why a figure is missing, when one is.
	Note string `json:"note,omitempty"`
}

func (s *Server) handlePortfolioHoldings(w http.ResponseWriter, r *http.Request) {
	address := AddressFrom(r.Context())
	if !common.IsHexAddress(address) {
		writeError(w, http.StatusBadRequest, "bad_address", "session address is not valid")
		return
	}
	trades, err := s.store.TradesForWallet(r.Context(), address)
	if err != nil {
		s.log.Error("load trades failed", "error", err)
		writeStoreError(w, err)
		return
	}

	type group struct {
		chainID chain.ID
		token   string
		trades  []store.Trade
	}
	var groups []*group
	byKey := map[string]*group{}
	for _, t := range trades {
		k := string(t.ChainID) + ":" + t.Token
		g, ok := byKey[k]
		if !ok {
			g = &group{chainID: t.ChainID, token: t.Token}
			byKey[k] = g
			groups = append(groups, g)
		}
		g.trades = append(g.trades, t)
	}

	hidden, err := s.store.HiddenHoldings(r.Context(), address)
	if err != nil {
		s.log.Error("load hidden holdings failed", "error", err)
		writeStoreError(w, err)
		return
	}
	shown := groups[:0]
	for _, g := range groups {
		at, ok := hidden[string(g.chainID)+":"+g.token]
		// A trade recorded after the token was hidden brings it back.
		if ok && !anyRecordedAfter(g.trades, at) {
			continue
		}
		shown = append(shown, g)
	}
	groups = shown

	owner := common.HexToAddress(address)
	results := make([]*Holding, len(groups))
	var wg sync.WaitGroup
	for i, g := range groups {
		wg.Add(1)
		go func(i int, g *group) {
			defer wg.Done()
			results[i] = s.holding(r.Context(), owner, g.chainID, g.trades)
		}(i, g)
	}
	wg.Wait()

	out := make([]Holding, 0, len(results))
	for _, h := range results {
		if h != nil {
			out = append(out, *h)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return valueOr(out[i].ValueUSD) > valueOr(out[j].ValueUSD)
	})
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func anyRecordedAfter(trades []store.Trade, at time.Time) bool {
	for _, t := range trades {
		if t.RecordedAt.After(at) {
			return true
		}
	}
	return false
}

// handleHideHolding removes a token from the session wallet's "Tokens bought"
// list. Its trades are kept; a later trade in the token lists it again.
func (s *Server) handleHideHolding(w http.ResponseWriter, r *http.Request) {
	address := AddressFrom(r.Context())
	token := strings.ToLower(chi.URLParam(r, "token"))
	chainID := chain.ID(chi.URLParam(r, "chainID"))
	if !common.IsHexAddress(address) {
		writeError(w, http.StatusBadRequest, "bad_address", "session address is not valid")
		return
	}
	if _, ok := s.reg.Get(chainID); !ok {
		writeError(w, http.StatusBadRequest, "unknown_chain", "no such chain")
		return
	}
	if !common.IsHexAddress(token) {
		writeError(w, http.StatusBadRequest, "bad_token", "token must be an address")
		return
	}
	if err := s.store.HideHolding(r.Context(), address, chainID, token); err != nil {
		s.log.Error("hide holding failed", "chain", chainID, "token", token, "error", err)
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func valueOr(p *float64) float64 {
	if p == nil {
		return -1
	}
	return *p
}

// holding builds one token's line, or nil when the wallet no longer holds it.
func (s *Server) holding(ctx context.Context, owner common.Address, chainID chain.ID, trades []store.Trade) *Holding {
	ch, ok := s.reg.Get(chainID)
	if !ok || len(trades) == 0 {
		return nil
	}
	latest := trades[len(trades)-1]
	p, err := s.store.GetPool(ctx, chainID, latest.Pool)
	if err != nil || p.Token0 == nil || p.Token1 == nil {
		return nil
	}
	o := chain.Orient(ch, p.Token0, p.Token1)
	token := *o.Base
	tokenAddr := common.HexToAddress(latest.Token)

	var balance *big.Int
	if chain.IsNative(latest.Token) {
		balance, err = s.nativeBalance(ctx, chainID, owner)
	} else {
		balance, _, err = s.tokenBalance(ctx, chainID, tokenAddr, owner)
	}
	if err != nil || balance == nil || balance.Sign() == 0 {
		return nil
	}

	h := &Holding{ChainID: chainID, Pool: p.Address, Token: token, Amount: balance.String(), Risk: p.Risk}
	if steps, err := s.approvalSteps(ctx, ch, p, owner, &token, balance, time.Now().Add(sellReadyFor)); err == nil {
		ready := len(steps) == 0
		h.SellReady = &ready
	}
	scale := pow10(token.Decimals)
	held, _ := new(big.Float).Quo(new(big.Float).SetInt(balance), scale).Float64()

	// Price: what a small sell into the pool pays now, in dollars per token.
	// The probe is a thousandth of the balance, so the figure is the market
	// price rather than the price of dumping the whole position.
	if price, ok := s.tokenPriceUSD(ctx, ch, p, o, balance); ok {
		value := price * held
		h.PriceUSD, h.ValueUSD = &price, &value
	}

	// Average cost from verified trades: buys add tokens at their cost, sells
	// remove tokens at the running average.
	bought, sold := new(big.Int), new(big.Int)
	var cost float64
	costKnown := true
	for _, t := range trades {
		amt, ok := new(big.Int).SetString(t.TokenAmount, 10)
		if !ok {
			continue
		}
		if t.Side == "buy" {
			bought.Add(bought, amt)
			if t.QuoteUSD == nil {
				costKnown = false
			} else {
				cost += *t.QuoteUSD
			}
		} else {
			sold.Add(sold, amt)
		}
	}
	if bought.Sign() == 0 {
		h.Note = "no recorded buy for this token"
		return h
	}
	if !costKnown {
		h.Note = "a buy was paid in an asset with no dollar price, so its cost is unknown"
		return h
	}
	boughtTokens, _ := new(big.Float).Quo(new(big.Float).SetInt(bought), scale).Float64()
	avg := cost / boughtTokens
	h.AvgEntryUSD = &avg

	// The balance may hold more than recorded trades explain: bought elsewhere,
	// or sent in. Their cost is unknown, so no P&L is better than a wrong one.
	net := new(big.Int).Sub(bought, sold)
	tolerance := new(big.Int).Div(net, big.NewInt(200)) // 0.5% for rounding and reflections
	if balance.Cmp(new(big.Int).Add(net, tolerance)) > 0 {
		h.Note = "this wallet holds more than it bought through EarlyPair; P&L covers only verified trades and is not shown"
		return h
	}
	if h.ValueUSD != nil {
		basis := avg * held
		pnl := *h.ValueUSD - basis
		h.PnlUSD = &pnl
		if basis > 0 {
			pct := pnl / basis * 100
			h.PnlPct = &pct
		}
	}
	return h
}

// tokenPriceUSD prices the traded token by quoting a small sell into its pool.
func (s *Server) tokenPriceUSD(ctx context.Context, ch chain.Chain, p chain.Pool, o chain.Orientation, balance *big.Int) (float64, bool) {
	probe := new(big.Int).Div(balance, big.NewInt(1000))
	if probe.Sign() == 0 {
		probe = new(big.Int).Set(balance)
	}
	out, err := s.sellQuote(ctx, ch, p, o, probe)
	if err != nil || out.Sign() <= 0 {
		return 0, false
	}
	usd, ok := s.quoteUSD(ctx, ch, o.Quote.Address)
	if !ok {
		return 0, false
	}
	in, _ := new(big.Float).Quo(new(big.Float).SetInt(probe), pow10(o.Base.Decimals)).Float64()
	received, _ := new(big.Float).Quo(new(big.Float).SetInt(out), pow10(o.Quote.Decimals)).Float64()
	if in <= 0 {
		return 0, false
	}
	return received * usd / in, true
}

// sellQuote is the quote received for selling amount of the traded token.
func (s *Server) sellQuote(ctx context.Context, ch chain.Chain, p chain.Pool, o chain.Orientation, amount *big.Int) (*big.Int, error) {
	if isV4Pool(p) {
		if ch.UniswapV4 == nil || ch.UniswapV4.Quoter == "" {
			return nil, errors.New("v4 not configured")
		}
		key, err := s.v4Key(ctx, ch, p)
		if err != nil {
			return nil, err
		}
		out, _, err := uniswapv4.Quote(ctx, s.pool.EVM(ch.ID), common.HexToAddress(ch.UniswapV4.Quoter), key,
			common.HexToAddress(o.Base.Address) == key.Currency0, amount)
		return out, err
	}
	if isV3Pool(ch, p) {
		if ch.UniswapV3 == nil || ch.UniswapV3.Quoter == "" || p.FeeTierBps == nil {
			return nil, errors.New("v3 not configured")
		}
		out, _, err := uniswapv3.Quote(ctx, s.pool.EVM(ch.ID), common.HexToAddress(ch.UniswapV3.Quoter),
			common.HexToAddress(o.Base.Address), common.HexToAddress(o.Quote.Address), v3Fee(p), amount)
		return out, err
	}
	if p.Model != chain.ModelV2 {
		return nil, fmt.Errorf("pricing %s pools is not supported", p.Model)
	}
	r0, r1, err := s.pairReserves(ctx, ch.ID, p.Address)
	if err != nil || r0 == nil || r1 == nil {
		return nil, errors.New("reserves unknown")
	}
	_, _, reserveIn, reserveOut := tradeSides(o, "sell", r0, r1)
	feeBps := 30
	if p.FeeTierBps != nil {
		feeBps = *p.FeeTierBps
	}
	return amountOutV2(amount, reserveIn, reserveOut, feeBps), nil
}
