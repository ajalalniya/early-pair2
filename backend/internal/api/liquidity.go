package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/go-chi/chi/v5"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/clmm"
)

// The liquidity distribution behind a pool's depth chart.
//
// Concentrated liquidity is not spread along the whole curve the way a v2 pair
// is: it sits in ranges someone chose, and where it sits decides what a trade
// costs. This reads it from the pool's own state — see internal/clmm — and
// caches it briefly, because the answer moves only when someone opens, closes
// or crosses a position, while a chart on screen would otherwise re-read it on
// every glance.

// liquidityTTL is how long a distribution is served from memory. Short enough
// that a position opened a moment ago shows up, long enough that a page open in
// several tabs costs one read.
const liquidityTTL = 20 * time.Second

type liquidityEntry struct {
	at   time.Time
	dist *liquidityResponse
}

// liquidityResponse is the distribution plus the two figures needed to read it
// in money rather than in L: what the quote asset is worth, and what the pool
// charges. A depth chart needs neither — but sizing a deposit against the
// liquidity already there ("what would a dollar earn in this range?") turns on
// both, and the caller cannot work either out from the bins alone.
type liquidityResponse struct {
	*clmm.Distribution
	// QuoteUSD is the dollar price of the token this pool is priced in, null
	// when it is not an asset with an established price.
	QuoteUSD *float64 `json:"quoteUsd"`
	// FeeBps is the pool's own fee tier, the share of each swap its liquidity
	// providers are paid.
	FeeBps *int `json:"feeBps"`
}

func (s *Server) handleLiquidity(w http.ResponseWriter, r *http.Request) {
	chainID := chain.ID(chi.URLParam(r, "chainID"))
	address := strings.ToLower(chi.URLParam(r, "pool"))

	ch, ok := s.reg.Get(chainID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown_chain", "no such chain")
		return
	}
	p, err := s.store.GetPool(r.Context(), chainID, address)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if p.Model != chain.ModelCLMM {
		writeError(w, http.StatusUnprocessableEntity, "not_concentrated",
			"this pool spreads its liquidity along the whole curve, so it has no distribution to chart")
		return
	}
	if p.Token0 == nil || p.Token1 == nil {
		writeError(w, http.StatusConflict, "not_enriched", "this pool's tokens are not resolved yet; try again in a moment")
		return
	}

	key := string(chainID) + ":" + address
	if v, ok := s.liquidity.Load(key); ok {
		if e := v.(liquidityEntry); time.Since(e.at) < liquidityTTL {
			writeJSON(w, http.StatusOK, e.dist)
			return
		}
	}

	ctx := r.Context()
	o := chain.Orient(ch, p.Token0, p.Token1)
	var pool clmm.Reader

	switch {
	case isV4Pool(p):
		if ch.UniswapV4 == nil || ch.UniswapV4.StateView == "" {
			writeError(w, http.StatusNotImplemented, "state_view_missing",
				"reading a Uniswap v4 pool's liquidity needs the StateView contract, which is not configured for this chain")
			return
		}
		k, err := s.v4Key(ctx, ch, p)
		if err != nil {
			s.log.Warn("v4 pool key unavailable", "pool", p.Address, "error", err)
			writeError(w, http.StatusBadGateway, "pool_key_unavailable",
				"this pool's parameters could not be read from the chain; try again in a moment")
			return
		}
		pool = clmm.V4PoolWithSpacing{
			V4Pool:      clmm.V4Pool{StateView: common.HexToAddress(ch.UniswapV4.StateView), PoolID: common.HexToHash(p.Address)},
			TickSpacing: int32(k.TickSpacing.Int64()),
		}
	case isV3Pool(ch, p):
		pool = clmm.V3Pool{Address: common.HexToAddress(p.Address)}
	default:
		writeError(w, http.StatusNotImplemented, "model_unsupported",
			"reading liquidity is implemented for Uniswap v3 and v4 pools")
		return
	}

	dist, err := clmm.Read(ctx, s.pool.EVM(ch.ID), common.HexToAddress(ch.Multicall3),
		pool, p.Token0.Decimals, p.Token1.Decimals, o.BaseIsToken0)
	if err != nil {
		s.log.Warn("liquidity read failed", "chain", ch.ID, "pool", p.Address, "error", err)
		writeError(w, http.StatusBadGateway, "liquidity_unavailable",
			"this pool's liquidity could not be read from the chain; try again")
		return
	}
	resp := &liquidityResponse{Distribution: dist, FeeBps: p.FeeTierBps}
	if usd, ok := s.quoteUSD(ctx, ch, o.Quote.Address); ok {
		resp.QuoteUSD = &usd
	}
	s.liquidity.Store(key, liquidityEntry{at: time.Now(), dist: resp})
	writeJSON(w, http.StatusOK, resp)
}
