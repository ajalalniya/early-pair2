// Package api is the HTTP and WebSocket surface. Public reads are cacheable
// and unauthenticated; anything tied to a wallet requires a session issued by
// signing a nonce, and nothing in here ever holds key material.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/config"
	"github.com/earlypair/backend/internal/risk"
	"github.com/earlypair/backend/internal/rpcpool"
	"github.com/earlypair/backend/internal/store"
	"github.com/earlypair/backend/internal/stream"
)

// Server holds the dependencies every handler needs.
type Server struct {
	cfg    config.Config
	reg    *chain.Registry
	store  *store.Store
	pool   *rpcpool.Pool
	hub    *stream.Hub
	scorer *risk.Scorer
	auth   *Auth
	log    *slog.Logger

	// bus links this process to the indexer: live events arrive over it, and
	// the health snapshot and operator actions go through it. Nil when Redis is
	// disabled, in which case those features fall back to this process's view.
	bus *stream.Bus

	// v4Keys caches Uniswap v4 PoolKeys by chain:poolId. A key never changes.
	v4Keys sync.Map
	// liquidity caches depth distributions by chain:pool for liquidityTTL. The
	// entries are small and bounded by the pools anyone actually opens.
	liquidity sync.Map
}

// SetBus attaches the cross-process event bus.
func (s *Server) SetBus(b *stream.Bus) { s.bus = b }

func NewServer(cfg config.Config, reg *chain.Registry, st *store.Store, pool *rpcpool.Pool,
	hub *stream.Hub, scorer *risk.Scorer, log *slog.Logger) *Server {
	return &Server{
		cfg:    cfg,
		reg:    reg,
		store:  st,
		pool:   pool,
		hub:    hub,
		scorer: scorer,
		auth:   NewAuth(cfg.Auth, st, log),
		log:    log.With("component", "api"),
	}
}

// Routes builds the router.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(s.logRequests)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(s.cors)

	r.Get("/healthz", s.handleHealth)
	r.Get("/readyz", s.handleReady)

	r.Route("/api/v1", func(r chi.Router) {
		// Public reads. Short cache is deliberate: the stream carries live
		// updates, so a couple of seconds of staleness on a cold load is free.
		r.Group(func(r chi.Router) {
			r.Use(s.publicCache)
			r.Get("/chains", s.handleChains)
			r.Get("/pools", s.handleListPools)
			r.Get("/pools/search", s.handleSearchPools)
			r.Get("/pools/{chainID}/{pool}", s.handleGetPool)
			r.Get("/pools/{chainID}/{pool}/risk", s.handlePoolRisk)
			r.Get("/pools/{chainID}/{pool}/traders", s.handlePoolTraders)
			r.Get("/pools/{chainID}/{pool}/liquidity", s.handleLiquidity)
		})

		// Adding a pool by address writes, so it sits outside the cached group.
		r.Post("/pools/import", s.handleImportPool)

		r.Get("/favorites", s.handleListFavorites)
		r.Put("/favorites/{chainID}/{pool}", s.handleAddFavorite)
		r.Delete("/favorites/{chainID}/{pool}", s.handleRemoveFavorite)

		r.Get("/rpc/health", s.handleRPCHealth)
		r.Post("/rpc/endpoints", s.handleAddEndpoint)
		r.Post("/rpc/endpoints/{id}/action", s.handleEndpointAction)

		r.Post("/auth/nonce", s.handleAuthNonce)
		r.Post("/auth/verify", s.handleAuthVerify)

		r.Post("/quote/swap", s.handleQuoteSwap)
		r.Post("/quote/zap", s.handleQuoteZap)
		r.Post("/quote/exit", s.handleQuoteExit)
		r.Post("/tx/build", s.handleTxBuild)
		r.Post("/tx/approve", s.handleApprove)
		r.Post("/tx/send", s.handleTxSend)
		// Public: the receipt proves who traded. Viewing holdings needs a session.
		r.Post("/portfolio/trades", s.handleRecordTrade)

		r.Group(func(r chi.Router) {
			r.Use(s.auth.Require)
			r.Get("/portfolio/funds", s.handlePortfolioFunds)
			r.Get("/portfolio/holdings", s.handlePortfolioHoldings)
			r.Delete("/portfolio/holdings/{chainID}/{token}", s.handleHideHolding)
			r.Get("/portfolio/positions", s.handlePortfolioPositions)

			r.Get("/watch/tokens", s.handleListTokenWatches)
			r.Post("/watch/tokens", s.handleAddTokenWatch)
			r.Delete("/watch/tokens/{id}", s.handleDeleteTokenWatch)

			r.Get("/watch/topics", s.handleListTopicWatches)
			r.Post("/watch/topics", s.handleAddTopicWatch)
			r.Delete("/watch/topics/{id}", s.handleDeleteTopicWatch)
		})

		r.Get("/stream", s.handleStream)
	})

	return r
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		// Debug level: at feed volumes an info-level access log is noise that
		// buries the operational events that matter.
		s.log.Debug("request",
			"method", r.Method, "path", r.URL.Path, "status", ww.Status(),
			"bytes", ww.BytesWritten(), "duration", time.Since(start),
			"requestId", middleware.GetReqID(r.Context()))
	})
}

func (s *Server) cors(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range s.cfg.Server.CORSOrigins {
		allowed[strings.TrimRight(o, "/")] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimRight(r.Header.Get("Origin"), "/")
		if origin != "" && allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) publicCache(next http.Handler) http.Handler {
	ttl := int(s.cfg.Server.PublicCacheTTL.Seconds())
	if ttl <= 0 {
		ttl = 2
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", ttl))
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Health(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ready",
		"streamClients": s.hub.Clients(),
	})
}

// ChainInfo is what /chains returns: everything the UI needs to render a chain
// filter chip without hardcoding a list.
type ChainInfo struct {
	ID           chain.ID   `json:"id"`
	Name         string     `json:"name"`
	Tag          string     `json:"tag"`
	Kind         chain.Kind `json:"kind"`
	NumericID    uint64     `json:"numericId,omitempty"`
	Explorer     string     `json:"explorer,omitempty"`
	NativeSymbol string     `json:"nativeSymbol,omitempty"`
	Head         uint64     `json:"head"`
	// Ingesting is false for configured chains whose ingester is not
	// implemented, so the UI can say so rather than show an empty feed that
	// looks like "no new pools".
	Ingesting bool `json:"ingesting"`
	// Stables and WrappedNative are the chain's quote assets. A pair is traded
	// in whichever side is one of them — a stable before the native currency —
	// which is how the UI tells the token being bought from what pays for it.
	Stables       []string `json:"stables"`
	WrappedNative string   `json:"wrappedNative,omitempty"`
	// Dexes names the DEXes whose pools this chain's ingester watches, in config
	// order, so the UI can offer a DEX filter without hardcoding a list that
	// goes stale the moment a factory is added here.
	Dexes []string `json:"dexes"`
}

func (s *Server) handleChains(w http.ResponseWriter, r *http.Request) {
	chains := s.reg.All()
	out := make([]ChainInfo, 0, len(chains))
	for _, c := range chains {
		out = append(out, ChainInfo{
			ID: c.ID, Name: c.Name, Tag: c.Tag, Kind: c.Kind,
			NumericID: c.NumericID, Explorer: c.Explorer, NativeSymbol: c.NativeSymbol,
			Head:      s.pool.FleetHead(c.ID),
			Ingesting: c.Kind == chain.KindEVM && len(chain.CreationTopics(c.Factories)) > 0,
			Stables:   lowerAll(c.Stables), WrappedNative: strings.ToLower(c.WrappedNative),
			Dexes: dexNames(c.Factories),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// dexNames lists each configured DEX once, keeping config order.
func dexNames(factories []chain.Factory) []string {
	out := make([]string, 0, len(factories))
	seen := map[string]bool{}
	for _, f := range factories {
		if f.Dex == "" || seen[f.Dex] {
			continue
		}
		seen[f.Dex] = true
		out = append(out, f.Dex)
	}
	return out
}

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = strings.ToLower(v)
	}
	return out
}

// --- response helpers ---

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already out; nothing useful left to do but note it.
		slog.Default().Debug("response encode failed", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorBody
	body.Error.Code = code
	body.Error.Message = message
	writeJSON(w, status, body)
}

// writeStoreError maps store failures onto status codes without leaking SQL.
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such record")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "request could not be completed")
	}
}

func decodeBody(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}

func queryInt(r *http.Request, key string) (int, bool) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return n, true
}

func queryFloat(r *http.Request, key string) (float64, bool) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}
