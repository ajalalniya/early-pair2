package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/go-chi/chi/v5"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/store"
)

// maxTopicWatchesPerUser is a hard product constraint, not a nicety. Every
// watched topic widens the chain-wide log sweep, and an unbounded topic on a
// busy chain — Transfer, say — would consume the entire public RPC budget for
// every user at once.
const maxTopicWatchesPerUser = 10

func (s *Server) handleListTokenWatches(w http.ResponseWriter, r *http.Request) {
	owner := AddressFrom(r.Context())
	watches, err := s.store.ListTokenWatches(r.Context(), owner)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	// Resolve each hit into the pool the UI needs to render the row.
	type hitJSON struct {
		ChainID chain.ID `json:"chainId"`
		Pool    string   `json:"pool"`
		Pair    string   `json:"pair"`
		Dex     string   `json:"dex"`
		SeenAt  string   `json:"seenAt"`
	}
	type watchJSON struct {
		store.TokenWatch
		ResolvedHits []hitJSON `json:"resolvedHits"`
	}

	out := make([]watchJSON, 0, len(watches))
	for _, watch := range watches {
		wj := watchJSON{TokenWatch: watch, ResolvedHits: []hitJSON{}}
		for _, h := range watch.Hits {
			hj := hitJSON{ChainID: h.ChainID, Pool: h.Pool, SeenAt: h.SeenAt.UTC().Format("2006-01-02T15:04:05Z")}
			if p, err := s.store.GetPool(r.Context(), h.ChainID, h.Pool); err == nil {
				hj.Pair, hj.Dex = p.Pair, p.Dex
			}
			wj.ResolvedHits = append(wj.ResolvedHits, hj)
		}
		out = append(out, wj)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

type addTokenWatchRequest struct {
	// Name is what the user typed: a symbol or an address.
	Name   string   `json:"name"`
	Chains []string `json:"chains,omitempty"`
}

func (s *Server) handleAddTokenWatch(w http.ResponseWriter, r *http.Request) {
	owner := AddressFrom(r.Context())

	var req addTokenWatchRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "empty_name", "give a token symbol or address to watch")
		return
	}
	for _, c := range req.Chains {
		if _, ok := s.reg.Get(chain.ID(c)); !ok {
			writeError(w, http.StatusBadRequest, "unknown_chain", "no such chain: "+c)
			return
		}
	}

	// An address is authoritative. A bare symbol is ambiguous by nature, so the
	// entry is created unresolved and the candidates are returned for the user
	// to narrow — guessing which GROK they meant is exactly the kind of silent
	// wrong answer that costs someone money.
	address := ""
	var candidates []map[string]any
	if common.IsHexAddress(name) {
		address = strings.ToLower(name)
	} else {
		found, err := s.store.ResolveTokenBySymbol(r.Context(), name)
		if err == nil && len(found) == 1 {
			address = found[0].Address
		} else if err == nil {
			for _, t := range found {
				candidates = append(candidates, map[string]any{
					"chainId": t.ChainID, "address": t.Address, "symbol": t.Symbol,
				})
			}
		}
	}

	watch, err := s.store.AddTokenWatch(r.Context(), owner, strings.ToUpper(name), address, req.Chains)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	resp := map[string]any{"watch": watch}
	if address == "" {
		resp["status"] = "resolving"
		resp["message"] = "watching by symbol; the address resolves when a matching token is indexed"
		if len(candidates) > 0 {
			resp["candidates"] = candidates
		}
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleDeleteTokenWatch(w http.ResponseWriter, r *http.Request) {
	owner := AddressFrom(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "id must be a number")
		return
	}
	if err := s.store.DeleteTokenWatch(r.Context(), owner, id); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListTopicWatches(w http.ResponseWriter, r *http.Request) {
	owner := AddressFrom(r.Context())
	watches, err := s.store.ListTopicWatches(r.Context(), owner)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":  watches,
		"limit": maxTopicWatchesPerUser,
	})
}

type addTopicWatchRequest struct {
	// Signature is either a human event signature or a raw 32-byte topic0.
	Signature string   `json:"signature"`
	Name      string   `json:"name,omitempty"`
	Chains    []string `json:"chains,omitempty"`
	Alerts    bool     `json:"alerts,omitempty"`
}

func (s *Server) handleAddTopicWatch(w http.ResponseWriter, r *http.Request) {
	owner := AddressFrom(r.Context())

	var req addTopicWatchRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	sig := strings.TrimSpace(req.Signature)
	if sig == "" {
		writeError(w, http.StatusBadRequest, "empty_signature",
			"give an event signature such as PairCreated(address,address,address,uint256), or a topic0 hash")
		return
	}

	count, err := s.store.CountTopicWatches(r.Context(), owner)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if count >= maxTopicWatchesPerUser {
		writeError(w, http.StatusTooManyRequests, "topic_limit",
			"you already watch the maximum number of topics; remove one first")
		return
	}

	watch := store.TopicWatch{Owner: owner, Chains: req.Chains, Alerts: req.Alerts}

	if strings.HasPrefix(sig, "0x") && len(sig) == 66 {
		watch.Topic0 = strings.ToLower(sig)
		watch.Name = req.Name
		if watch.Name == "" {
			watch.Name = sig[:10] + "…"
		}
	} else {
		normalized := chain.NormalizeSignature(sig)
		if !strings.Contains(normalized, "(") || !strings.HasSuffix(normalized, ")") {
			writeError(w, http.StatusBadRequest, "bad_signature",
				"signature must look like Name(type,type) or be a 0x-prefixed topic0 hash")
			return
		}
		watch.Signature = normalized
		watch.Topic0 = strings.ToLower(chain.TopicForSignature(sig).Hex())
		watch.Name = req.Name
		if watch.Name == "" {
			watch.Name = normalized[:strings.Index(normalized, "(")]
		}
	}

	for _, c := range req.Chains {
		if _, ok := s.reg.Get(chain.ID(c)); !ok {
			writeError(w, http.StatusBadRequest, "unknown_chain", "no such chain: "+c)
			return
		}
	}

	created, err := s.store.AddTopicWatch(r.Context(), watch)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"watch": created,
		"note":  "a topic that matches more than the rate ceiling is suspended automatically and shown as such",
	})
}

func (s *Server) handleDeleteTopicWatch(w http.ResponseWriter, r *http.Request) {
	owner := AddressFrom(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "id must be a number")
		return
	}
	if err := s.store.DeleteTopicWatch(r.Context(), owner, id); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
