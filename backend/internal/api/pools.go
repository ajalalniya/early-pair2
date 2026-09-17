package api

import (
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/store"
)

func (s *Server) handleListPools(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	f := store.PoolFilter{
		ChainID: chain.ID(q.Get("chain")),
		Risk:    q.Get("risk"),
		Sort:    q.Get("sort"),
		Cursor:  q.Get("cursor"),
	}
	if n, ok := queryInt(r, "limit"); ok {
		f.Limit = n
	}
	if v, ok := queryFloat(r, "minLiq"); ok {
		f.MinLiq = &v
	}
	if n, ok := queryInt(r, "maxAgeMin"); ok {
		f.MaxAgeMin = &n
	}
	switch f.Risk {
	case "", "all", "safe", "watch", "danger":
	default:
		writeError(w, http.StatusBadRequest, "bad_risk_filter",
			`risk must be one of: safe, watch, danger`)
		return
	}
	if f.ChainID != "" && f.ChainID != "all" {
		if _, ok := s.reg.Get(f.ChainID); !ok {
			writeError(w, http.StatusBadRequest, "unknown_chain",
				"no such chain: "+string(f.ChainID))
			return
		}
	}

	pools, next, err := s.store.ListPools(r.Context(), f)
	if err != nil {
		if strings.Contains(err.Error(), "bad cursor") {
			writeError(w, http.StatusBadRequest, "bad_cursor", "cursor is not valid")
			return
		}
		s.log.Error("list pools failed", "error", err)
		writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"data":       s.poolsResponse(pools),
		"nextCursor": next,
	})
}

// searchQuery is a pool address or its prefix: 0x and at least four hex digits,
// up to a v4 PoolId's 64. Anything shorter matches too much to be a search.
var searchQuery = regexp.MustCompile(`^0x[0-9a-f]{4,64}$`)

func (s *Server) handleSearchPools(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if !searchQuery.MatchString(q) {
		writeError(w, http.StatusBadRequest, "bad_query",
			"q must be a pair address or its start: 0x followed by at least 4 hex digits")
		return
	}
	pools, err := s.store.SearchPools(r.Context(), q, 20)
	if err != nil {
		s.log.Error("search pools failed", "error", err)
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": s.poolsResponse(pools)})
}

func (s *Server) handleGetPool(w http.ResponseWriter, r *http.Request) {
	chainID := chain.ID(chi.URLParam(r, "chainID"))
	address := chi.URLParam(r, "pool")

	p, err := s.store.GetPool(r.Context(), chainID, address)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.poolResponse(p))
}

func (s *Server) handlePoolRisk(w http.ResponseWriter, r *http.Request) {
	chainID := chain.ID(chi.URLParam(r, "chainID"))
	address := chi.URLParam(r, "pool")

	p, err := s.store.GetPool(r.Context(), chainID, address)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if p.Risk == nil {
		// Not yet scored. Saying so is the honest answer; inventing a neutral
		// score would read as a clean bill of health.
		writeJSON(w, http.StatusOK, map[string]any{
			"scored":  false,
			"message": "this pool has not been scored yet",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scored": true,
		"risk":   p.Risk,
		"gated":  s.scorer.Gated(p.Risk.Score, 0),
	})
}

func (s *Server) handlePoolTraders(w http.ResponseWriter, r *http.Request) {
	chainID := chain.ID(chi.URLParam(r, "chainID"))
	address := chi.URLParam(r, "pool")

	p, err := s.store.GetPool(r.Context(), chainID, address)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	// These are the market tracker's figures: complete from the pair's creation
	// while it is tracked, then frozen at the end of its window.
	resp := map[string]any{
		"buyers":        p.Buyers1h,
		"sellers":       p.Sellers1h,
		"pending":       p.TradersPending,
		"volumeUsd":     p.Volume1hUSD,
		"priceChange":   p.PriceChange1h,
		"liquidityUsd":  p.LiquidityUSD,
		"metricsUntil":  s.metricsUntil(p),
		"metricsFrozen": s.metricsFrozen(p),
	}
	if p.Buyers1h != nil && p.Sellers1h != nil {
		b, sl := *p.Buyers1h, *p.Sellers1h
		switch {
		case sl > 0:
			resp["ratio"] = float64(b) / float64(sl)
		case b > 0:
			resp["ratio"] = nil
			resp["ratioInfinite"] = true
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// metricsUntil is when a pool's market columns stop updating: its creation plus
// the tracking window. Nil for pools found by a catch-up, which were never
// tracked and whose market columns stay empty.
func (s *Server) metricsUntil(p chain.Pool) *time.Time {
	if p.FavoritedAt != nil {
		return nil // watched for as long as it stays starred
	}
	if p.RemovedAt != nil {
		return p.RemovedAt
	}
	if p.Backfilled || s.cfg.Ingest.TrackWindow <= 0 {
		return nil
	}
	t := p.CreatedAt.Add(s.cfg.Ingest.TrackWindow)
	return &t
}

func (s *Server) metricsFrozen(p chain.Pool) bool {
	if p.FavoritedAt != nil {
		return false
	}
	until := s.metricsUntil(p)
	return until == nil || time.Now().After(*until)
}

// poolResponse is the wire shape. It exists so the JSON contract is defined in
// one place rather than falling out of whatever the domain struct happens to
// look like.
type poolJSON struct {
	ChainID chain.ID    `json:"chainId"`
	Address string      `json:"address"`
	Pair    string      `json:"pair"`
	Dex     string      `json:"dex"`
	Model   chain.Model `json:"model"`
	State   chain.State `json:"state"`

	CreatedAt    time.Time `json:"createdAt"`
	CreatedBlock uint64    `json:"createdBlock"`
	CreatedTx    string    `json:"createdTx"`
	Backfilled   bool      `json:"backfilled,omitempty"`

	Token0 *chain.Token `json:"token0"`
	Token1 *chain.Token `json:"token1"`

	FeeTierBps *int `json:"feeTierBps"`
	// Hooks is set for Uniswap v4 pools with a hook contract. The UI should
	// show it: a hook can take fees or block sells on every swap.
	Hooks string `json:"hooks,omitempty"`

	LiquidityUSD  *float64 `json:"liquidityUsd"`
	Volume1hUSD   *float64 `json:"volume1hUsd"`
	PriceChange1h *float64 `json:"priceChange1h"`
	Buyers1h      *int     `json:"buyers1h"`
	Sellers1h     *int     `json:"sellers1h"`
	// TradersPending is how many swaps still have no known trader; while it is
	// above zero, buyers1h and sellers1h are a floor ("46+"), not a final count.
	TradersPending *int `json:"tradersPending"`

	Risk *chain.RiskSummary `json:"risk"`

	// MetricsUntil is when liquidity, volume, buyers/sellers and price change
	// stop updating: a new pair is tracked for a fixed window after creation,
	// then its figures describe those opening minutes. MetricsFrozen says the
	// window has passed. A pair found by a catch-up has no window and no figures.
	MetricsUntil  *time.Time `json:"metricsUntil"`
	MetricsFrozen bool       `json:"metricsFrozen"`

	// ScreenedAt is set once the pair cleared (or failed) the feed's bar.
	// RemovedAt is set on a pair that failed; only a direct lookup returns one.
	ScreenedAt *time.Time `json:"screenedAt"`
	RemovedAt  *time.Time `json:"removedAt,omitempty"`
	// FavoritedAt is set on a starred pair, which is watched until unstarred.
	FavoritedAt *time.Time `json:"favoritedAt"`
	// ImportedAt is set on a pair a user added by address rather than one the
	// sweep found live, so the UI can say so: its figures begin when it was
	// added, not when the pair launched.
	ImportedAt *time.Time `json:"importedAt,omitempty"`
}

func (s *Server) poolResponse(p chain.Pool) poolJSON {
	return poolJSON{
		ChainID: p.ChainID, Address: p.Address, Pair: p.Pair, Dex: p.Dex,
		Model: p.Model, State: p.State,
		CreatedAt: p.CreatedAt, CreatedBlock: p.CreatedBlock, CreatedTx: p.CreatedTx,
		Backfilled: p.Backfilled,
		Token0:     p.Token0, Token1: p.Token1,
		FeeTierBps:   p.FeeTierBps,
		Hooks:        p.Hooks,
		LiquidityUSD: p.LiquidityUSD, Volume1hUSD: p.Volume1hUSD,
		PriceChange1h: p.PriceChange1h,
		Buyers1h:      p.Buyers1h, Sellers1h: p.Sellers1h,
		TradersPending: p.TradersPending,
		Risk:           p.Risk,
		MetricsUntil:   s.metricsUntil(p),
		MetricsFrozen:  s.metricsFrozen(p),
		ScreenedAt:     p.ScreenedAt,
		RemovedAt:      p.RemovedAt,
		FavoritedAt:    p.FavoritedAt,
		ImportedAt:     p.ImportedAt,
	}
}

func (s *Server) poolsResponse(pools []chain.Pool) []poolJSON {
	out := make([]poolJSON, 0, len(pools))
	for _, p := range pools {
		out = append(out, s.poolResponse(p))
	}
	return out
}
