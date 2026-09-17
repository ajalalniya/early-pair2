package rpcpool

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/rpc"
	"golang.org/x/time/rate"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/config"
	"github.com/earlypair/backend/internal/obs"
)

// CostClass separates cheap calls from log sweeps so that scanning history can
// never starve the quote path of its rate budget on the same endpoint.
type CostClass int

const (
	ClassCheap CostClass = iota
	ClassNormal
	ClassExpensive
)

func (c CostClass) String() string {
	switch c {
	case ClassCheap:
		return "cheap"
	case ClassExpensive:
		return "expensive"
	default:
		return "normal"
	}
}

// tokens is what a class costs against the endpoint's bucket. Expensive calls
// cost more so a burst of getLogs self-throttles before the provider does it
// for us with a 429.
func (c CostClass) tokens() int {
	switch c {
	case ClassCheap:
		return 1
	case ClassExpensive:
		return 4
	default:
		return 2
	}
}

// Capability names a query shape that some providers refuse on their free
// tier. Refusals are tracked per endpoint and per shape, so an endpoint that
// will not serve address-less log queries is still used for everything else.
type Capability string

const (
	// CapLogs is any eth_getLogs query. Refused outright by endpoints that gate
	// log access, or that treat a range slightly behind the tip as "archive".
	CapLogs Capability = "logs"
	// CapLogsNoAddress is an eth_getLogs query with no address filter, which
	// several public endpoints reject to protect themselves from full scans.
	CapLogsNoAddress Capability = "logs_without_address"
)

// cooldown is how long a refusal is remembered before the endpoint is asked
// again. An address filter requirement is a fixed policy, so it is remembered
// for an hour; an archive refusal depends on how far behind the tip the query
// was, so it is retried sooner.
func (c Capability) cooldown() time.Duration {
	if c == CapLogsNoAddress {
		return time.Hour
	}
	return 5 * time.Minute
}

// strikesToRefuse is how many refusals of a plain log query it takes before an
// endpoint is routed around. Publicnode, for one, load-balances across backends
// that keep different amounts of history: measured live, the same endpoint
// served logs 300 and 2,500 blocks behind the tip and refused them 1,000 blocks
// back. Barring it on a single refusal left Ethereum with no endpoint at all.
// An address-filter requirement is a fixed policy, so one refusal is enough.
func (c Capability) strikesToRefuse() int {
	if c == CapLogsNoAddress {
		return 1
	}
	return 3
}

// refusal is what an endpoint is known to refuse. minDepth is the shallowest
// depth behind the chain head at which it has refused: an archive refusal says
// nothing about queries near the tip, which the same endpoint serves fine.
type refusal struct {
	until       time.Time
	minDepth    uint64
	strikes     int
	firstStrike time.Time
}

// Status is the health snapshot the /rpc/health endpoint renders. Field names
// mirror what the UI panel displays.
type Status struct {
	ID             string          `json:"id"`
	ChainID        chain.ID        `json:"chainId"`
	Transport      chain.Transport `json:"transport"`
	URL            string          `json:"url"` // masked, always
	State          string          `json:"state"`
	LatencyMS      *int            `json:"latencyMs"`
	P50MS          *int            `json:"p50Ms"`
	P95MS          *int            `json:"p95Ms"`
	Block          *uint64         `json:"block"`
	LagBlocks      uint64          `json:"lagBlocks"`
	Subscriptions  int             `json:"subscriptions"`
	UptimeSec      *int64          `json:"uptimeSec"`
	Reconnects24h  int             `json:"reconnects24h"`
	PollIntervalMS int             `json:"pollIntervalMs,omitempty"`
	DemotedUntil   *time.Time      `json:"demotedUntil,omitempty"`
	LastError      string          `json:"lastError,omitempty"`
	SuccessRate    float64         `json:"successRate"`
	// Limitations lists query shapes this endpoint is currently refusing, so
	// the health panel can explain why a healthy-looking endpoint is skipped.
	Limitations []Capability `json:"limitations,omitempty"`
}

// Endpoint state names, matching the three the UI knows how to render.
const (
	StateHealthy     = "HEALTHY"
	StateDegraded    = "DEGRADED"
	StateUnreachable = "UNREACHABLE"
)

// Endpoint is one RPC provider. All mutable health state is behind mu; the hot
// path (pick an endpoint, record a result) takes it briefly and never blocks on
// IO while holding it.
type Endpoint struct {
	ID        string
	ChainID   chain.ID
	Transport chain.Transport
	Masked    string
	Weight    int

	rawURL string // never logged, never serialised

	mu sync.Mutex

	client   *rpc.Client
	dialErr  error
	dialedAt time.Time

	lim       *rate.Limiter
	bgLim     *rate.Limiter // background work's share; see Background
	baseRate  float64
	curRate   float64
	limitedAt time.Time

	lat        *ring
	ok, failed int
	consecFail int
	lastErr    string

	breakerUntil time.Time

	head         uint64
	headAt       time.Time
	lagBlocks    uint64
	laggingSince time.Time

	// WebSocket-only state.
	subs         int
	connectedAt  time.Time
	drops        []time.Time
	demotedUntil time.Time

	// logRange is the adaptive getLogs chunk size this provider has proven it
	// will serve. It shrinks on rejection and creeps back up on success.
	logRange    uint64
	maxLogRange uint64

	// refused records query shapes the provider will not serve, until when.
	// Kept separate from the breaker on purpose: an endpoint that answers
	// eth_blockNumber perfectly while refusing address-less getLogs is healthy,
	// and every successful head check would otherwise reset a breaker that
	// method-specific refusals could then never trip.
	refused map[Capability]*refusal

	policy config.RPCPolicy
}

func newEndpoint(e config.Endpoint, ch chain.Chain, policy config.RPCPolicy) *Endpoint {
	r := e.RateLimit
	if r <= 0 {
		r = 5 // conservative default for an unspecified free endpoint
	}
	burst := e.Burst
	if burst <= 0 {
		burst = int(r) + 4
	}
	// A bucket that cannot hold one expensive request would reject it outright
	// rather than delay it, so the floor is the largest class cost.
	if burst < ClassExpensive.tokens() {
		burst = ClassExpensive.tokens()
	}
	maxRange := e.MaxLogRange
	if maxRange == 0 {
		maxRange = ch.MaxLogRange
	}
	if maxRange == 0 {
		maxRange = 500
	}
	return &Endpoint{
		ID:          e.ID,
		ChainID:     e.ChainID,
		Transport:   e.Transport,
		Masked:      config.MaskURL(e.URL),
		Weight:      e.Weight,
		rawURL:      e.URL,
		lim:         rate.NewLimiter(rate.Limit(r), burst),
		bgLim:       rate.NewLimiter(rate.Limit(r*backgroundShare), burst),
		baseRate:    r,
		curRate:     r,
		lat:         newRing(64),
		logRange:    maxRange,
		maxLogRange: maxRange,
		policy:      policy,
	}
}

// available reports whether this endpoint may take a request right now.
func (e *Endpoint) available(now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.availableLocked(now)
}

func (e *Endpoint) availableLocked(now time.Time) bool {
	// Breaker open: nothing goes out. Once it cools, requests flow again and
	// the first result either resets consecFail or re-opens the breaker — that
	// is the half-open probe, without a separate state to leak.
	if now.Before(e.breakerUntil) {
		return false
	}
	// An endpoint that has been trailing the fleet for too long is out of
	// rotation even though its individual requests still succeed.
	if !e.laggingSince.IsZero() && now.Sub(e.laggingSince) > e.policy.HeadLagEvictFor {
		return false
	}
	return true
}

// score ranks endpoints for selection. Higher is better. Latency dominates,
// success rate gates, configured weight breaks ties.
func (e *Endpoint) score(now time.Time) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()

	p50 := e.lat.percentile(0.5)
	if p50 <= 0 {
		p50 = 250 * time.Millisecond // unmeasured: assume mediocre, not perfect
	}
	s := 1000.0 / float64(p50.Milliseconds()+1)

	total := e.ok + e.failed
	if total > 0 {
		s *= float64(e.ok) / float64(total)
	}
	if e.lagBlocks > 0 {
		s /= float64(e.lagBlocks + 1)
	}
	s += float64(e.Weight)
	if e.consecFail > 0 {
		s /= float64(e.consecFail + 1)
	}
	return s
}

// backgroundShare is the most of an endpoint's rate that background work may
// take. The rest is always left for pool discovery and market tracking.
const backgroundShare = 0.5

type backgroundKey struct{}

// Background marks calls made with ctx as background work, which draws on at
// most backgroundShare of each endpoint's rate budget.
//
// Without it, enrichment and discovery queued on one limiter as equals. On a
// chain served by a single 5 req/s endpoint, enrichment's log probes took
// nearly the whole budget, the discovery cursor fell 40 minutes behind, and
// every pair it found was too old to be market-tracked.
func Background(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

func isBackground(ctx context.Context) bool {
	v, _ := ctx.Value(backgroundKey{}).(bool)
	return v
}

// wait applies the endpoint's rate limit for a request of the given class.
// Background calls first pass their own, smaller limiter, then take their
// tokens from the shared one like everything else.
func (e *Endpoint) wait(ctx context.Context, class CostClass) error {
	e.mu.Lock()
	lim, bg := e.lim, e.bgLim
	e.mu.Unlock()
	if isBackground(ctx) {
		if err := bg.WaitN(ctx, class.tokens()); err != nil {
			return err
		}
	}
	return lim.WaitN(ctx, class.tokens())
}

// setRate applies a rate to both limiters. Callers hold e.mu.
func (e *Endpoint) setRate(r float64) {
	e.lim.SetLimit(rate.Limit(r))
	e.bgLim.SetLimit(rate.Limit(r * backgroundShare))
}

// dial lazily opens the JSON-RPC client. WebSocket endpoints are dialled by the
// subscription manager instead, so this only ever runs for HTTP.
func (e *Endpoint) dial(ctx context.Context) (*rpc.Client, error) {
	e.mu.Lock()
	if e.client != nil {
		c := e.client
		e.mu.Unlock()
		return c, nil
	}
	e.mu.Unlock()

	c, err := rpc.DialOptions(ctx, e.rawURL)
	if err != nil {
		e.recordFailure(fmt.Errorf("dial: %w", err))
		return nil, err
	}

	e.mu.Lock()
	// Another goroutine may have won the race; keep one client.
	if e.client != nil {
		other := e.client
		e.mu.Unlock()
		c.Close()
		return other, nil
	}
	e.client = c
	e.dialedAt = time.Now()
	e.mu.Unlock()
	return c, nil
}

func (e *Endpoint) recordSuccess(method string, d time.Duration) {
	e.mu.Lock()
	e.lat.add(d)
	e.ok++
	e.consecFail = 0
	e.lastErr = ""
	e.breakerUntil = time.Time{}
	e.mu.Unlock()

	obs.RPCRequests.WithLabelValues(e.ID, method, "ok").Inc()
	obs.RPCLatency.WithLabelValues(e.ID, method).Observe(d.Seconds())
}

func (e *Endpoint) recordFailure(err error) {
	e.mu.Lock()
	e.failed++
	e.consecFail++
	e.lastErr = truncate(err.Error(), 200)
	if e.consecFail >= e.policy.BreakerFailures {
		e.breakerUntil = time.Now().Add(e.policy.BreakerProbeAfter)
	}
	// A connection-level failure invalidates the cached client so the next
	// attempt redials rather than reusing a dead socket forever.
	if isConnError(err) && e.client != nil {
		e.client.Close()
		e.client = nil
	}
	e.mu.Unlock()
}

// rateLimited applies AIMD: halve the allowed rate now, recover linearly.
//
// The burst is deliberately left alone. Shrinking it below the cost of the most
// expensive request class would make those requests fail instantly and forever
// with "exceeds limiter's burst" — not a delay but a permanent rejection, since
// a bucket that can never hold four tokens can never serve a four-token call.
// Holding requests off for the provider's Retry-After is the breaker's job.
func (e *Endpoint) rateLimited(retryAfter time.Duration) {
	e.mu.Lock()
	e.curRate = maxFloat(0.2, e.curRate/2)
	e.setRate(e.curRate)
	e.limitedAt = time.Now()
	if retryAfter > 0 {
		e.breakerUntil = time.Now().Add(retryAfter)
	}
	e.mu.Unlock()
	obs.RPCRateLimited.WithLabelValues(e.ID).Inc()
}

// recoverRate creeps the limit back toward the configured baseline. Called from
// the pool's janitor; five minutes from halved to full.
func (e *Endpoint) recoverRate(step time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.curRate >= e.baseRate || e.limitedAt.IsZero() {
		return
	}
	const recoverOver = 5 * time.Minute
	inc := e.baseRate * float64(step) / float64(recoverOver)
	e.curRate = minFloat(e.baseRate, e.curRate+inc)
	e.setRate(e.curRate)
	if e.curRate >= e.baseRate {
		e.limitedAt = time.Time{}
	}
}

// noteHead records a head observation from either a poll or a socket push.
func (e *Endpoint) noteHead(n uint64) {
	e.mu.Lock()
	if n > e.head {
		e.head = n
	}
	e.headAt = time.Now()
	e.mu.Unlock()
}

// noteLag updates how far this endpoint trails the fleet, starting or clearing
// the eviction timer.
func (e *Endpoint) noteLag(fleetHead uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.head == 0 || fleetHead <= e.head {
		e.lagBlocks = 0
		e.laggingSince = time.Time{}
		return
	}
	e.lagBlocks = fleetHead - e.head
	if e.lagBlocks > e.policy.HeadLagEvictBlocks {
		if e.laggingSince.IsZero() {
			e.laggingSince = time.Now()
		}
	} else {
		e.laggingSince = time.Time{}
	}
}

// ShrinkLogRange halves the working chunk after a provider rejects a range.
func (e *Endpoint) ShrinkLogRange() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.logRange > 1 {
		e.logRange /= 2
	}
	obs.LogRangeShrinks.WithLabelValues(e.ID).Inc()
	return e.logRange
}

// GrowLogRange creeps the chunk back up after sustained success, so a transient
// rejection does not permanently cripple throughput.
func (e *Endpoint) GrowLogRange() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.logRange < e.maxLogRange {
		e.logRange += e.logRange/4 + 1
		if e.logRange > e.maxLogRange {
			e.logRange = e.maxLogRange
		}
	}
}

func (e *Endpoint) LogRange() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.logRange
}

// noteWSConnected / noteWSDrop maintain the socket lifecycle counters the
// health panel shows, and enforce the "HTTP fallback after 2 drops" rule the UI
// advertises to users.
func (e *Endpoint) noteWSConnected(subs int) {
	e.mu.Lock()
	e.connectedAt = time.Now()
	e.subs = subs
	e.consecFail = 0
	e.lastErr = ""
	e.mu.Unlock()
}

func (e *Endpoint) noteWSDrop(err error) (demoted bool, until time.Time) {
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()

	e.subs = 0
	e.connectedAt = time.Time{}
	if err != nil {
		e.lastErr = truncate(err.Error(), 200)
	}
	e.drops = append(e.drops, now)
	// Keep a 24h window for the panel's reconnect count.
	cut := now.Add(-24 * time.Hour)
	kept := e.drops[:0]
	for _, d := range e.drops {
		if d.After(cut) {
			kept = append(kept, d)
		}
	}
	e.drops = kept

	recent := 0
	window := now.Add(-e.policy.WSDropWindow)
	for _, d := range e.drops {
		if d.After(window) {
			recent++
		}
	}
	if recent >= e.policy.WSDropsBeforeDemote && now.After(e.demotedUntil) {
		e.demotedUntil = now.Add(e.policy.WSDemoteFor)
		return true, e.demotedUntil
	}
	return false, e.demotedUntil
}

func (e *Endpoint) wsDemoted(now time.Time) (bool, time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return now.Before(e.demotedUntil), e.demotedUntil
}

func (e *Endpoint) reconnects24h() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.drops)
}

// Status renders the endpoint for the health API.
func (e *Endpoint) Status(fleetHead uint64, pollMS int) Status {
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()

	st := Status{
		ID:            e.ID,
		ChainID:       e.ChainID,
		Transport:     e.Transport,
		URL:           e.Masked,
		Subscriptions: e.subs,
		LagBlocks:     e.lagBlocks,
		Reconnects24h: len(e.drops),
		LastError:     e.lastErr,
	}
	if e.Transport == chain.HTTP {
		st.PollIntervalMS = pollMS
	}
	if p50 := e.lat.percentile(0.5); p50 > 0 {
		v := int(p50.Milliseconds())
		st.P50MS = &v
	}
	if p95 := e.lat.percentile(0.95); p95 > 0 {
		v := int(p95.Milliseconds())
		st.P95MS = &v
	}
	if last := e.lat.last(); last > 0 {
		v := int(last.Milliseconds())
		st.LatencyMS = &v
	}
	if e.head > 0 {
		h := e.head
		st.Block = &h
	}
	if !e.connectedAt.IsZero() {
		up := int64(now.Sub(e.connectedAt).Seconds())
		st.UptimeSec = &up
	}
	if now.Before(e.demotedUntil) {
		d := e.demotedUntil
		st.DemotedUntil = &d
	}
	if total := e.ok + e.failed; total > 0 {
		st.SuccessRate = float64(e.ok) / float64(total)
	}
	for c, r := range e.refused {
		if now.Before(r.until) {
			st.Limitations = append(st.Limitations, c)
		}
	}
	sort.Slice(st.Limitations, func(i, j int) bool { return st.Limitations[i] < st.Limitations[j] })

	switch {
	case now.Before(e.breakerUntil), e.consecFail >= e.policy.BreakerFailures:
		st.State = StateUnreachable
	case e.Transport == chain.WS && e.connectedAt.IsZero():
		st.State = StateUnreachable
	case e.lagBlocks > 0, now.Before(e.demotedUntil), len(recentDrops(e.drops, now, time.Hour)) > 0:
		st.State = StateDegraded
	case st.P50MS != nil && *st.P50MS > 500:
		st.State = StateDegraded
	default:
		st.State = StateHealthy
	}
	return st
}

func recentDrops(drops []time.Time, now time.Time, window time.Duration) []time.Time {
	cut := now.Add(-window)
	var out []time.Time
	for _, d := range drops {
		if d.After(cut) {
			out = append(out, d)
		}
	}
	return out
}

// Refuses reports whether this endpoint is known to refuse a query shape.
func (e *Endpoint) Refuses(c Capability, depth uint64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.refused[c]
	return ok && time.Now().Before(r.until) && depth >= r.minDepth
}

// MarkRefused records a refusal of a query shape at a depth behind the chain
// head (how many blocks back the query started). The endpoint is only routed
// around once refusals repeat, and then only for queries at least as deep as
// the shallowest one it refused. It reports whether the endpoint has just
// become refused, so callers can log that once rather than on every sweep.
func (e *Endpoint) MarkRefused(c Capability, depth uint64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	obs.CapabilityRefusals.WithLabelValues(e.ID, string(c)).Inc()

	if e.refused == nil {
		e.refused = map[Capability]*refusal{}
	}
	now := time.Now()
	r, ok := e.refused[c]
	if !ok || (now.After(r.until) && now.Sub(r.firstStrike) > c.cooldown()) {
		r = &refusal{firstStrike: now, minDepth: depth}
		e.refused[c] = r
	}
	wasActive := now.Before(r.until)

	r.strikes++
	if c == CapLogsNoAddress {
		r.minDepth = 0 // a policy, not a history limit
	} else if depth < r.minDepth {
		r.minDepth = depth
	}
	if r.strikes >= c.strikesToRefuse() {
		r.until = now.Add(c.cooldown())
	}
	return !wasActive && now.Before(r.until)
}

// ClearPenalties resets the breaker, the lag eviction and any socket demotion.
//
// It backs the health panel's Resubscribe and Failover actions. Rather than
// forcing a dial — which would race the socket supervisor into holding two
// connections to the same endpoint — it clears what is holding the endpoint
// back and lets the normal reconnect path proceed on its next cycle.
func (e *Endpoint) ClearPenalties() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.breakerUntil = time.Time{}
	e.consecFail = 0
	e.laggingSince = time.Time{}
	e.demotedUntil = time.Time{}
	e.refused = nil
	e.lastErr = ""
	e.curRate = e.baseRate
	e.setRate(e.baseRate)
	e.lim.SetBurst(int(e.baseRate) + 4)
	e.limitedAt = time.Time{}
}

// close releases the endpoint's client.
func (e *Endpoint) close() {
	e.mu.Lock()
	c := e.client
	e.client = nil
	e.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

// ring is a fixed-size latency window.
type ring struct {
	buf  []time.Duration
	i    int
	full bool
}

func newRing(n int) *ring { return &ring{buf: make([]time.Duration, n)} }

func (r *ring) add(d time.Duration) {
	r.buf[r.i] = d
	r.i = (r.i + 1) % len(r.buf)
	if r.i == 0 {
		r.full = true
	}
}

func (r *ring) last() time.Duration {
	n := len(r.buf)
	idx := (r.i - 1 + n) % n
	return r.buf[idx]
}

func (r *ring) percentile(p float64) time.Duration {
	n := len(r.buf)
	if !r.full {
		n = r.i
	}
	if n == 0 {
		return 0
	}
	cp := make([]time.Duration, n)
	copy(cp, r.buf[:n])
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(float64(n-1) * p)
	return cp[idx]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func isConnError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, frag := range []string{
		"connection refused", "connection reset", "broken pipe", "eof",
		"no such host", "i/o timeout", "tls", "use of closed network connection",
		"websocket: close", "unexpected eof",
	} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
