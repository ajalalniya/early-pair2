package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/store"
	"github.com/earlypair/backend/internal/stream"
)

// Favorites are shared, not per wallet: starring needs no sign-in, and what a
// favorite buys — the market tracker watching the pair until it is unstarred —
// is spent from the one RPC budget everyone shares. The list is capped for the
// same reason.

func (s *Server) handleListFavorites(w http.ResponseWriter, r *http.Request) {
	pools, err := s.store.ListFavorites(r.Context())
	if err != nil {
		s.log.Error("list favorites failed", "error", err)
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":  s.poolsResponse(pools),
		"limit": store.MaxFavorites,
	})
}

func (s *Server) handleAddFavorite(w http.ResponseWriter, r *http.Request) {
	chainID := chain.ID(chi.URLParam(r, "chainID"))
	address := strings.ToLower(chi.URLParam(r, "pool"))

	if err := s.store.AddFavorite(r.Context(), chainID, address); err != nil {
		if errors.Is(err, store.ErrFavoritesFull) {
			writeError(w, http.StatusConflict, "favorites_full",
				fmt.Sprintf("at most %d pairs can be favorites; unstar one first", store.MaxFavorites))
			return
		}
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("add favorite failed", "chain", chainID, "pool", address, "error", err)
		}
		writeStoreError(w, err)
		return
	}
	p, err := s.store.GetPool(r.Context(), chainID, address)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.publishFavorite(p.ChainID, p.Address, p.FavoritedAt)
	writeJSON(w, http.StatusOK, s.poolResponse(p))
}

func (s *Server) handleRemoveFavorite(w http.ResponseWriter, r *http.Request) {
	chainID := chain.ID(chi.URLParam(r, "chainID"))
	address := strings.ToLower(chi.URLParam(r, "pool"))

	if err := s.store.RemoveFavorite(r.Context(), chainID, address); err != nil {
		s.log.Error("remove favorite failed", "chain", chainID, "pool", address, "error", err)
		writeStoreError(w, err)
		return
	}
	s.publishFavorite(chainID, address, nil)
	w.WriteHeader(http.StatusNoContent)
}

// publishFavorite tells every connected client a pair was starred or
// unstarred, in the partial pools.update shape state changes already use.
func (s *Server) publishFavorite(chainID chain.ID, address string, at *time.Time) {
	s.hub.Publish(stream.TopicPoolsUpdate, chainID, map[string]any{
		"chainId":     chainID,
		"address":     strings.ToLower(address),
		"favoritedAt": at,
	})
}
