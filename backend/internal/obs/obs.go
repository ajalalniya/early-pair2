// Package obs wires structured logging and the Prometheus metrics that the
// operational requirements call for. Everything is registered once at init so
// any package can record without threading a collector through its API.
package obs

import (
	"log/slog"
	"os"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// PairsDetected counts pools that made it through decode and dedupe.
	PairsDetected = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pairs_detected_total",
		Help: "New pools detected and accepted, by chain.",
	}, []string{"chain"})

	// PairsScreened counts the market tracker's verdicts on new pairs.
	PairsScreened = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pairs_screened_total",
		Help: "New pairs judged against the feed's liquidity and volume bar, by chain and outcome (kept, removed).",
	}, []string{"chain", "outcome"})

	// DetectionLatency measures block timestamp to client-frame emission. This
	// is the number the whole ingestion design exists to keep small.
	DetectionLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "detection_latency_seconds",
		Help:    "Seconds from block timestamp to pool emitted on the stream.",
		Buckets: []float64{0.25, 0.5, 1, 2, 3, 5, 10, 30, 60, 300},
	}, []string{"chain"})

	// CursorLag is how far behind head the persisted cursor is. Alert on this:
	// a rising cursor lag is the earliest signal that ingestion is failing.
	CursorLag = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cursor_lag_blocks",
		Help: "Blocks between the persisted ingest cursor and the fleet head.",
	}, []string{"chain"})

	RPCRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rpc_requests_total",
		Help: "RPC requests by endpoint, method and outcome.",
	}, []string{"endpoint", "method", "status"})

	RPCLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "rpc_request_duration_seconds",
		Help:    "RPC request latency by endpoint and method.",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
	}, []string{"endpoint", "method"})

	RPCRateLimited = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rpc_rate_limited_total",
		Help: "Requests rejected by a provider's rate limiter.",
	}, []string{"endpoint"})

	WSReconnects = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ws_reconnects_total",
		Help: "WebSocket reconnects by endpoint.",
	}, []string{"endpoint"})

	WSDemotions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ws_demotions_total",
		Help: "Endpoints demoted to HTTP-only after repeated socket drops.",
	}, []string{"endpoint"})

	EnrichmentQueueDepth = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "enrichment_queue_depth",
		Help: "Pending enrichment jobs, by stage.",
	}, []string{"stage"})

	Reorgs = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "reorgs_total",
		Help: "Chain reorganisations observed by the ingester.",
	}, []string{"chain"})

	// CapabilityRefusals counts providers refusing a query shape on their tier
	// (address-less or "archive" getLogs). A rise means a provider changed its
	// free-tier policy and traffic is shifting onto the rest of the fleet.
	CapabilityRefusals = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rpc_capability_refusals_total",
		Help: "Requests refused because the endpoint will not serve that query shape.",
	}, []string{"endpoint", "capability"})

	// WatchSweepsSkipped counts ranges where watched-topic logs could not be
	// fetched because no endpoint would serve an address-less query. Pool
	// detection is unaffected; topic watches on that chain miss the range.
	WatchSweepsSkipped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "watch_topic_sweeps_skipped_total",
		Help: "Block ranges where watched topics could not be swept.",
	}, []string{"chain"})

	LogRangeShrinks = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "getlogs_range_shrinks_total",
		Help: "Times a getLogs range was bisected after a provider rejection.",
	}, []string{"endpoint"})

	StreamClients = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "stream_clients",
		Help: "Connected WebSocket clients.",
	})

	StreamDropped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "stream_dropped_clients_total",
		Help: "Clients disconnected for failing to keep up with their send queue.",
	})

	BusDropped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "bus_messages_dropped_total",
		Help: "Events that could not be published to the cross-process bus.",
	})

	BusGaps = promauto.NewCounter(prometheus.CounterOpts{
		Name: "bus_sequence_gaps_total",
		Help: "Missed bus events detected by sequence number; each triggers a client resync.",
	})

	StreamFrames = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "stream_frames_total",
		Help: "Frames published to the stream hub, by topic.",
	}, []string{"topic"})
)

// SetupLogger installs a structured logger as the slog default. Text in dev so
// a terminal stays readable; JSON everywhere else so fields survive shipping.
func SetupLogger(level, format string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}

	var h slog.Handler
	if strings.EqualFold(format, "text") {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	l := slog.New(h)
	slog.SetDefault(l)
	return l
}
