package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/config"
	"github.com/earlypair/backend/internal/rpcpool"
	"github.com/earlypair/backend/internal/stream"
)

func (s *Server) handleRPCHealth(w http.ResponseWriter, r *http.Request) {
	// The endpoints that matter are the indexer's: they carry pool detection.
	// This process's own pool only serves quotes and balances, so reporting it
	// as "the" health would show sockets it never opened as unreachable and
	// hide refusals it never saw.
	var (
		statuses []rpcpool.Status
		source   = "indexer"
		asOf     time.Time
		note     string
	)
	if s.bus != nil {
		if raw, at, ok := s.bus.Health(r.Context()); ok {
			if err := json.Unmarshal(raw, &statuses); err == nil {
				asOf = at
			} else {
				statuses = nil
			}
		}
	}
	if statuses == nil {
		statuses = s.pool.Status()
		source = "api"
		asOf = time.Now().UTC()
		note = "no recent health snapshot from the indexer; showing only this API process's own " +
			"connections, which do not include ingestion sockets"
	}

	healthy, sockets, liveSockets, subs := 0, 0, 0, 0
	var latencies []int
	for _, st := range statuses {
		if st.State == "HEALTHY" {
			healthy++
		}
		if st.Transport == chain.WS {
			sockets++
			if st.State != "UNREACHABLE" {
				liveSockets++
			}
			subs += st.Subscriptions
		}
		if st.P50MS != nil && st.State != "UNREACHABLE" {
			latencies = append(latencies, *st.P50MS)
		}
	}

	// The median across the fleet, which is the number the UI's summary pill
	// shows. Computed here so every client agrees on it.
	var medianP50 *int
	if len(latencies) > 0 {
		for i := range latencies {
			for j := i + 1; j < len(latencies); j++ {
				if latencies[j] < latencies[i] {
					latencies[i], latencies[j] = latencies[j], latencies[i]
				}
			}
		}
		m := latencies[len(latencies)/2]
		medianP50 = &m
	}

	resp := map[string]any{
		"endpoints": statuses,
		"source":    source,
		"asOf":      asOf,
		"summary": map[string]any{
			"healthy":        healthy,
			"total":          len(statuses),
			"medianP50Ms":    medianP50,
			"socketsLive":    liveSockets,
			"socketsTotal":   sockets,
			"subscriptions":  subs,
			"failoverPolicy": "auto-resubscribe on socket close, HTTP fallback after 2 drops",
		},
	}
	if note != "" {
		resp["note"] = note
	}
	writeJSON(w, http.StatusOK, resp)
}

type addEndpointRequest struct {
	ChainID   chain.ID        `json:"chainId"`
	Transport chain.Transport `json:"transport"`
	URL       string          `json:"url"`
	RateLimit float64         `json:"rateLimit,omitempty"`
}

func (s *Server) handleAddEndpoint(w http.ResponseWriter, r *http.Request) {
	var req addEndpointRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	ch, ok := s.reg.Get(req.ChainID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown_chain", "no such chain: "+string(req.ChainID))
		return
	}
	switch req.Transport {
	case chain.HTTP:
		if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
			writeError(w, http.StatusBadRequest, "bad_url", "http transport needs an http(s) url")
			return
		}
	case chain.WS:
		if !strings.HasPrefix(req.URL, "ws://") && !strings.HasPrefix(req.URL, "wss://") {
			writeError(w, http.StatusBadRequest, "bad_url", "ws transport needs a ws(s) url")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "bad_transport", `transport must be "http" or "ws"`)
		return
	}

	// A user-supplied endpoint gets a conservative rate budget by default: we
	// have no idea what tier it is, and finding out by getting banned is the
	// expensive way.
	rate := req.RateLimit
	if rate <= 0 || rate > 50 {
		rate = 5
	}

	id := string(req.ChainID) + "-" + strings.ToLower(string(req.Transport)) + "-" + shortHash(req.URL)
	cfgEndpoint := config.Endpoint{
		ID: id, ChainID: req.ChainID, Transport: req.Transport,
		URL: req.URL, RateLimit: rate,
	}

	ep, err := s.pool.Add(cfgEndpoint, ch)
	if err != nil {
		writeError(w, http.StatusConflict, "duplicate_endpoint", err.Error())
		return
	}

	if err := s.store.AddRPCEndpoint(r.Context(), id, req.ChainID, req.Transport, req.URL, rate); err != nil {
		s.log.Warn("endpoint added to the pool but not persisted", "endpoint", id, "error", err)
	}

	// The masked form is the only one that leaves this process.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":        ep.ID,
		"chainId":   ep.ChainID,
		"transport": ep.Transport,
		"url":       ep.Masked,
		"note":      "a newly added websocket endpoint is subscribed on the next ingester restart",
	})
}

type endpointActionRequest struct {
	Action string `json:"action"`
}

func (s *Server) handleEndpointAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ep, ok := s.pool.ByID(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}

	var req endpointActionRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	switch req.Action {
	case "ping", "resubscribe", "failover":
	default:
		writeError(w, http.StatusBadRequest, "bad_action",
			`action must be one of: resubscribe, ping, failover`)
		return
	}

	// With an indexer on the bus, the action goes to the endpoint that carries
	// ingestion. Its effect shows up in the next health snapshot.
	if s.bus != nil {
		delivered, err := s.bus.SendControl(r.Context(), stream.Control{Action: req.Action, EndpointID: id})
		if err != nil {
			writeError(w, http.StatusBadGateway, "bus_unavailable", "could not reach the indexer: "+err.Error())
			return
		}
		if !delivered {
			writeError(w, http.StatusServiceUnavailable, "no_indexer",
				"no indexer is listening, so there is no ingestion endpoint to act on")
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"ok":     true,
			"action": req.Action,
			"note":   "sent to the indexer; the result appears in the health snapshot within about 5 seconds",
		})
		return
	}

	switch req.Action {
	case "ping":
		head, err := s.pool.PingEndpoint(r.Context(), ep)
		if err != nil {
			writeError(w, http.StatusBadGateway, "ping_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "head": head})

	case "resubscribe", "failover":
		// Both amount to the same thing: clear the endpoint's penalty state so
		// the supervisor's next attempt is not held back, and let the normal
		// reconnect path do the work. There is no separate "force" path,
		// because a forced dial that races the supervisor is how you end up
		// with two sockets on one endpoint.
		ep.ClearPenalties()
		writeJSON(w, http.StatusAccepted, map[string]any{
			"ok":     true,
			"action": req.Action,
			"note":   "penalties cleared; the supervisor reconnects on its next cycle",
		})

	default:
		writeError(w, http.StatusBadRequest, "bad_action",
			`action must be one of: resubscribe, ping, failover`)
	}
}

func shortHash(s string) string {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		out[i] = hexDigits[h&0xf]
		h >>= 4
	}
	return string(out)
}
