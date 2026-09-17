package rpcpool

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/config"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// silentSocketServer accepts a WebSocket, answers the subscription handshake so
// everything looks healthy, and then never sends another frame.
//
// This is the failure mode that makes subscriptions untrustworthy on public
// infrastructure: the connection stays OPEN, the client has no error to react
// to, and blocks quietly stop arriving. Anything that treats an open socket as
// a working one will wait forever.
func silentSocketServer(t *testing.T, connections *int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
			Subprotocols:       []string{"jsonrpc"},
		})
		if err != nil {
			return
		}
		atomic.AddInt64(connections, 1)
		defer conn.Close(websocket.StatusNormalClosure, "")

		ctx := r.Context()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(data, &req); err != nil {
				return
			}
			if !strings.HasSuffix(req.Method, "subscribe") {
				continue
			}
			// Acknowledge the subscription, then go silent forever.
			resp, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  "0x1234567890abcdef",
			})
			if err := conn.Write(ctx, websocket.MessageText, resp); err != nil {
				return
			}
		}
	}))
}

func TestSilentSocketIsTreatedAsDead(t *testing.T) {
	// The liveness window is floored at fifteen seconds so a genuinely quiet
	// chain is not mistaken for a dead socket, which makes this test slow by
	// construction.
	if testing.Short() {
		t.Skip("takes ~16s: the silence window is floored at 15s")
	}
	var connections int64
	srv := silentSocketServer(t, &connections)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ch := chain.Chain{
		ID: "test", Name: "Test", Kind: chain.KindEVM,
		// A short block time makes the silence window short: the supervisor
		// gives up after three block times, floored at fifteen seconds.
		BlockTime: time.Second,
		Enabled:   true,
	}
	reg := chain.NewRegistry([]chain.Chain{ch})

	policy := config.Default().RPC
	policy.WSBackoffMin = 50 * time.Millisecond
	policy.WSBackoffMax = 100 * time.Millisecond
	policy.WSDropsBeforeDemote = 2
	policy.WSDropWindow = time.Minute
	policy.WSDemoteFor = 100 * time.Millisecond

	pool, err := New(policy, []config.Endpoint{
		{ID: "socket", ChainID: ch.ID, Transport: chain.WS, URL: wsURL, RateLimit: 100},
		// A chain needs an HTTP endpoint to exist in the pool at all; it is
		// never contacted in this test.
		{ID: "http", ChainID: ch.ID, Transport: chain.HTTP, URL: srv.URL, RateLimit: 100},
	}, reg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}

	ep, ok := pool.ByID("socket")
	if !ok {
		t.Fatal("socket endpoint not registered")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	bell := pool.StartSubscriptions(ctx, ch, SubSpec{})

	// The doorbell must never ring: the socket delivers nothing.
	select {
	case n := <-bell.C:
		t.Fatalf("a silent socket rang the doorbell with block %d", n)
	case <-time.After(2 * time.Second):
	}

	// It should still be reported connected at this point — which is exactly
	// why the liveness timer exists rather than relying on connection state.
	if st := ep.Status(0, 0); st.State == StateUnreachable {
		t.Log("socket already marked unreachable before the silence window elapsed")
	}

	// Wait out the silence window (3 block times, floored at 15s) plus slack,
	// and confirm the supervisor gave up and reconnected rather than waiting
	// on a connection that looked fine.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if ep.reconnects24h() > 0 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

	if got := ep.reconnects24h(); got == 0 {
		t.Fatal("a socket that stayed open and silent was never treated as dead; " +
			"blocks would stop arriving with nothing to react to")
	}

	// And it must come back rather than give up: the endpoint may simply have
	// been quiet, and abandoning it would cost the latency advantage for good.
	redialed := time.Now().Add(5 * time.Second)
	for time.Now().Before(redialed) {
		if atomic.LoadInt64(&connections) >= 2 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("the supervisor never redialled after giving up; got %d connections",
		atomic.LoadInt64(&connections))
}

func TestRateLimitDoesNotWedgeExpensiveCalls(t *testing.T) {
	// A 429 halves the endpoint's rate. It must not shrink the burst below the
	// cost of the most expensive request class, because a bucket that can never
	// hold that many tokens rejects those calls outright instead of delaying
	// them — a permanent failure dressed up as throttling.
	ch := chain.Chain{ID: "test", Kind: chain.KindEVM, Enabled: true}
	reg := chain.NewRegistry([]chain.Chain{ch})

	pool, err := New(config.Default().RPC, []config.Endpoint{
		{ID: "e1", ChainID: ch.ID, Transport: chain.HTTP, URL: "http://127.0.0.1:1", RateLimit: 1, Burst: 1},
	}, reg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ep, _ := pool.ByID("e1")

	ep.rateLimited(0)
	ep.rateLimited(0)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := ep.wait(ctx, ClassExpensive); err != nil {
		if strings.Contains(err.Error(), "exceeds limiter's burst") {
			t.Fatalf("throttling permanently wedged expensive calls: %v", err)
		}
		t.Fatalf("unexpected wait error: %v", err)
	}
}

// TestBackgroundWorkLeavesDiscoveryItsShare: background callers draining an
// endpoint as fast as they can must still leave foreground calls about half
// its rate, rather than queueing them behind the backlog as equals.
func TestBackgroundWorkLeavesDiscoveryItsShare(t *testing.T) {
	ch := chain.Chain{ID: "test", Kind: chain.KindEVM, Enabled: true}
	reg := chain.NewRegistry([]chain.Chain{ch})
	pool, err := New(config.Default().RPC, []config.Endpoint{
		{ID: "e1", ChainID: ch.ID, Transport: chain.HTTP, URL: "http://127.0.0.1:1", RateLimit: 40, Burst: 4},
	}, reg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ep, _ := pool.ByID("e1")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	bgCtx := Background(ctx)

	var bg, fg atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ { // a backlog of background work
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ep.wait(bgCtx, ClassCheap) == nil {
				bg.Add(1)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ep.wait(ctx, ClassCheap) == nil {
			fg.Add(1)
		}
	}()
	wg.Wait()

	// Over two seconds at 40/s: background is capped near 40, foreground gets
	// the remainder, near 40. Wide margins keep this stable on a loaded machine.
	if got := bg.Load(); got > 52 {
		t.Errorf("background took %d tokens, want at most about half of 80", got)
	}
	if got := fg.Load(); got < 25 {
		t.Errorf("foreground got %d tokens behind a background backlog, want about 40", got)
	}
}

func TestMaskURLHidesCredentials(t *testing.T) {
	cases := map[string]string{
		"https://eth-mainnet.g.alchemy.com/v2/AbCdEf1234567890xyz": "https://eth-mainnet.g.alchemy.com/v2/••••",
		"https://mainnet.infura.io/v3/0123456789abcdef0123456789":  "https://mainnet.infura.io/v3/••••",
		"https://rpc.example.com?apikey=secret12345":               "https://rpc.example.com?••••",
		"https://user:pass@rpc.example.com/x":                      "https://****@rpc.example.com/x",
		"https://mainnet.base.org":                                 "https://mainnet.base.org",
	}
	for in, want := range cases {
		if got := config.MaskURL(in); got != want {
			t.Errorf("MaskURL(%q) = %q, want %q", in, got, want)
		}
	}
}
