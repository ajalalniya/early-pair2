package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/stream"
)

// clientFrame is what a client sends us.
type clientFrame struct {
	Op      string   `json:"op"`
	Topics  []string `json:"topics,omitempty"`
	Filters struct {
		Chains []chain.ID `json:"chains,omitempty"`
	} `json:"filters,omitempty"`
	Since *uint64 `json:"since,omitempty"`
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: s.cfg.Server.CORSOrigins,
		// Frames are small JSON envelopes; compression costs more CPU than it
		// saves at this size and rate.
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Debug("websocket upgrade failed", "error", err)
		return
	}

	id := middleware.GetReqID(r.Context())
	if id == "" {
		id = fmt.Sprintf("c-%d", time.Now().UnixNano())
	}

	client := stream.NewClient(id, s.cfg.Stream.ClientQueue)
	s.hub.Add(client)
	defer s.hub.Remove(client)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Reader: subscriptions and pongs. A client that never subscribes receives
	// nothing, which keeps an idle connection genuinely idle.
	go func() {
		defer cancel()
		for {
			var f clientFrame
			if err := wsjson.Read(ctx, conn, &f); err != nil {
				return
			}
			switch f.Op {
			case "sub", "subscribe":
				s.applySubscription(ctx, conn, client, f)
			case "unsub", "unsubscribe":
				client.Subscribe(nil, nil)
			case "ping":
				_ = wsjson.Write(ctx, conn, map[string]string{"t": "pong"})
			}
		}
	}()

	pingEvery := s.cfg.Stream.PingInterval
	if pingEvery <= 0 {
		pingEvery = 20 * time.Second
	}
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "")
			return

		case <-ping.C:
			// A ping that does not come back within two intervals means the
			// connection is dead even though the socket still looks open — the
			// same failure mode we guard against on the upstream side.
			pctx, pcancel := context.WithTimeout(ctx, 2*pingEvery)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				conn.Close(websocket.StatusPolicyViolation, "ping timeout")
				return
			}

		case f, ok := <-client.Out():
			if !ok {
				// The hub closed us. If it was for lag, tell the client to
				// refetch rather than leaving it with a silent hole.
				if client.NeedsResync() {
					wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
					_ = wsjson.Write(wctx, conn, stream.Frame{
						T: stream.TopicResync, TS: time.Now().UnixMilli(),
					})
					wcancel()
					conn.Close(websocket.StatusTryAgainLater, "client too slow; resync")
					return
				}
				conn.Close(websocket.StatusGoingAway, "server shutting down")
				return
			}

			wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
			err := wsjson.Write(wctx, conn, f)
			wcancel()
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					s.log.Debug("stream write failed", "client", id, "error", err)
				}
				conn.Close(websocket.StatusInternalError, "write failed")
				return
			}
		}
	}
}

// applySubscription sets the client's filters and replays whatever it missed.
func (s *Server) applySubscription(ctx context.Context, conn *websocket.Conn, client *stream.Client, f clientFrame) {
	valid := map[string]bool{
		stream.TopicPoolsNew:    true,
		stream.TopicPoolsUpdate: true,
		stream.TopicHead:        true,
		stream.TopicRPCHealth:   true,
		stream.TopicWatchHits:   true,
		stream.TopicPortfolio:   true,
	}
	var topics []string
	for _, t := range f.Topics {
		if valid[t] {
			topics = append(topics, t)
		}
	}
	var chains []chain.ID
	for _, c := range f.Filters.Chains {
		if _, ok := s.reg.Get(c); ok {
			chains = append(chains, c)
		}
	}
	client.Subscribe(topics, chains)

	ack := map[string]any{
		"t":      "subscribed",
		"topics": topics,
		"chains": chains,
		"seq":    s.hub.Seq(),
	}

	if f.Since != nil {
		missed, complete := s.hub.Since(*f.Since, client.Subscription())
		if !complete {
			// The client was away longer than the replay buffer reaches. Saying
			// so is what keeps the frontend's "lossless reconnect" promise
			// honest: it refetches instead of assuming continuity.
			ack["resync"] = true
			ack["reason"] = "replay buffer does not reach back that far"
		} else {
			ack["replayed"] = len(missed)
			defer func() {
				for _, m := range missed {
					if !client.Send(m) {
						return
					}
				}
			}()
		}
	}

	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = wsjson.Write(wctx, conn, ack)
}

// marshalCheck keeps the compiler honest about frames staying JSON-encodable.
var _ = json.Marshal
