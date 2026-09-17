// Package config loads the service configuration: chain descriptors and their
// DEX factories from YAML, RPC endpoints from YAML plus env overrides, and the
// tunable risk weights. Anything an operator needs to change without a rebuild
// lives here.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/earlypair/backend/internal/chain"
)

// Config is the whole service configuration.
type Config struct {
	Server    Server              `yaml:"server"`
	Database  Database            `yaml:"database"`
	Redis     Redis               `yaml:"redis"`
	Log       Log                 `yaml:"log"`
	Chains    []chain.Chain       `yaml:"chains"`
	Endpoints []Endpoint          `yaml:"endpoints"`
	RPC       RPCPolicy           `yaml:"rpc"`
	Risk      RiskWeights         `yaml:"risk"`
	Ingest    Ingest              `yaml:"ingest"`
	Stream    Stream              `yaml:"stream"`
	Auth      Auth                `yaml:"auth"`
	Explorers map[string]Explorer `yaml:"explorers"`
}

type Server struct {
	Addr            string        `yaml:"addr"`
	MetricsAddr     string        `yaml:"metricsAddr"`
	ReadTimeout     time.Duration `yaml:"readTimeout"`
	WriteTimeout    time.Duration `yaml:"writeTimeout"`
	ShutdownTimeout time.Duration `yaml:"shutdownTimeout"`
	CORSOrigins     []string      `yaml:"corsOrigins"`
	// PublicCacheTTL is the Cache-Control max-age on public read endpoints.
	PublicCacheTTL time.Duration `yaml:"publicCacheTtl"`
}

type Database struct {
	URL         string        `yaml:"url"`
	MaxConns    int32         `yaml:"maxConns"`
	MinConns    int32         `yaml:"minConns"`
	MaxConnIdle time.Duration `yaml:"maxConnIdle"`
	AutoMigrate bool          `yaml:"autoMigrate"`
}

type Redis struct {
	URL      string `yaml:"url"`
	Disabled bool   `yaml:"disabled"`
}

type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Endpoint is one configured RPC provider.
type Endpoint struct {
	ID        string          `yaml:"id"`
	ChainID   chain.ID        `yaml:"chain"`
	Transport chain.Transport `yaml:"transport"`
	URL       string          `yaml:"url"`
	// RateLimit is requests per second allowed to this endpoint. Public tiers
	// are stingy; guessing high here is how you get soft-banned.
	RateLimit float64 `yaml:"rateLimit"`
	Burst     int     `yaml:"burst"`
	// Weight biases endpoint selection when health scores tie (higher wins).
	Weight int `yaml:"weight"`
	// MaxLogRange overrides the chain default for providers with tighter caps.
	MaxLogRange uint64 `yaml:"maxLogRange"`
	Disabled    bool   `yaml:"disabled"`
}

// RPCPolicy holds the pool-wide behaviour knobs.
type RPCPolicy struct {
	HedgeAfter        time.Duration `yaml:"hedgeAfter"`
	MaxHedges         int           `yaml:"maxHedges"`
	RequestTimeout    time.Duration `yaml:"requestTimeout"`
	BreakerFailures   int           `yaml:"breakerFailures"`
	BreakerProbeAfter time.Duration `yaml:"breakerProbeAfter"`
	// HeadLagEvictBlocks pulls an endpoint from rotation when it trails the
	// fleet median by this many blocks for HeadLagEvictFor.
	HeadLagEvictBlocks uint64        `yaml:"headLagEvictBlocks"`
	HeadLagEvictFor    time.Duration `yaml:"headLagEvictFor"`
	// WS demotion: this many drops inside WSDropWindow sends an endpoint to
	// HTTP-only for WSDemoteFor. Mirrors what the UI tells users happens.
	WSDropsBeforeDemote int           `yaml:"wsDropsBeforeDemote"`
	WSDropWindow        time.Duration `yaml:"wsDropWindow"`
	WSDemoteFor         time.Duration `yaml:"wsDemoteFor"`
	WSBackoffMin        time.Duration `yaml:"wsBackoffMin"`
	WSBackoffMax        time.Duration `yaml:"wsBackoffMax"`
}

// Ingest holds the poller's tunables.
type Ingest struct {
	// MaxBackfillBlocks caps how far a cold cursor will catch up before it
	// simply jumps to head. Prevents a week-old cursor from burning the budget.
	MaxBackfillBlocks uint64 `yaml:"maxBackfillBlocks"`
	// ResumeOnStart makes a start resume from the stored cursor and sweep the
	// blocks missed while stopped (up to MaxBackfillBlocks). Off, a start begins
	// at the head.
	ResumeOnStart    bool          `yaml:"resumeOnStart"`
	BackfillChunk    uint64        `yaml:"backfillChunk"`
	DoorbellDebounce time.Duration `yaml:"doorbellDebounce"`
	// ConfirmCheckEvery is how often unconfirmed pools are re-verified.
	ConfirmCheckEvery time.Duration `yaml:"confirmCheckEvery"`
	EnrichWorkers     int           `yaml:"enrichWorkers"`
	// TrackWindow is how long a new pair's market columns (liquidity, volume,
	// traders, price change) are kept up to date after its creation; then they
	// freeze. TrackEvery is how often tracked pairs are refreshed.
	TrackWindow     time.Duration `yaml:"trackWindow"`
	TrackEvery      time.Duration `yaml:"trackEvery"`
	Screen          Screen        `yaml:"screen"`
	EnrichQueueSize int           `yaml:"enrichQueueSize"`
	// PoolRetention is how long a pool is kept after its creation. Older pools
	// are deleted with their reserves, risk history and swaps, except starred
	// pools and pools someone traded. Zero keeps every pool forever.
	PoolRetention time.Duration `yaml:"poolRetention"`
}

// Screen is the bar a new pair must clear to stay in the feed, judged After
// its creation. A pair that fails is removed; one that clears it is tracked
// for the rest of TrackWindow. After of zero turns screening off.
type Screen struct {
	After           time.Duration `yaml:"after"`
	MinLiquidityUSD float64       `yaml:"minLiquidityUsd"`
	MinVolumeUSD    float64       `yaml:"minVolumeUsd"`
	// KeepIf is "both" (the default: removed when liquidity or volume falls
	// short) or "either" (removed only when both fall short).
	KeepIf string `yaml:"keepIf"`
}

type Stream struct {
	ReplayWindow    time.Duration `yaml:"replayWindow"`
	ReplayMaxFrames int           `yaml:"replayMaxFrames"`
	ClientQueue     int           `yaml:"clientQueue"`
	PingInterval    time.Duration `yaml:"pingInterval"`
	CoalesceWindow  time.Duration `yaml:"coalesceWindow"`
}

type Auth struct {
	JWTSecret  string        `yaml:"jwtSecret"`
	SessionTTL time.Duration `yaml:"sessionTtl"`
	NonceTTL   time.Duration `yaml:"nonceTtl"`
	Domain     string        `yaml:"domain"`
}

// Explorer is per-chain block explorer API access, used only for source
// verification. Without a key we report "unverified" rather than guessing.
type Explorer struct {
	API    string `yaml:"api"`
	APIKey string `yaml:"apiKey"`
}

// RiskWeights are the scorer's tunables. Keeping them in config means a rug
// pattern can be reweighted without a deploy, and the weights are auditable.
type RiskWeights struct {
	SellersThin int `yaml:"sellersThin"`
	RatioHigh   int `yaml:"ratioHigh"`
	LPUnlocked  int `yaml:"lpUnlocked"`
	// LPShortLock applies when the LP is locked but expires soon. The check
	// still passes — the lock is real — while the score reflects the cliff.
	LPShortLock int `yaml:"lpShortLock"`
	MintActive  int `yaml:"mintActive"`
	TaxNonZero  int `yaml:"taxNonZero"`
	TaxPunitive int `yaml:"taxPunitive"`
	// Top-10 concentration is graded rather than binary: 26% and 70% of supply
	// in ten hands are not the same danger, even though both fail the check.
	TopHeavyMild      int `yaml:"topHeavyMild"`
	TopHeavy          int `yaml:"topHeavy"`
	TopHeavySevere    int `yaml:"topHeavySevere"`
	TopHeavyExtreme   int `yaml:"topHeavyExtreme"`
	FewHolders        int `yaml:"fewHolders"`
	Unverified        int `yaml:"unverified"`
	HoneypotSimFailed int `yaml:"honeypotSimFailed"`
	// PoolFeeExtreme applies to a pool charging 25% or more on every trade, a
	// Uniswap v4 setting two thirds of new Robinhood pools use. A round trip at
	// 80% loses 96%, which is a rug in all but name, so like a reverting sell it
	// has to clear the gate threshold on its own.
	PoolFeeExtreme int `yaml:"poolFeeExtreme"`
	// GateThreshold is the default score at or above which the API refuses to
	// build a buy. Clients may raise it for themselves but never lower exits.
	GateThreshold int `yaml:"gateThreshold"`
}

// Default returns a configuration that runs against public endpoints with no
// API keys. It is the baseline every YAML file is merged onto.
func Default() Config {
	return Config{
		Server: Server{
			Addr:            ":8080",
			MetricsAddr:     ":9090",
			ReadTimeout:     15 * time.Second,
			WriteTimeout:    30 * time.Second,
			ShutdownTimeout: 20 * time.Second,
			CORSOrigins:     []string{"http://localhost:5173"},
			PublicCacheTTL:  2 * time.Second,
		},
		Database: Database{
			URL:         "postgres://earlypair:earlypair@localhost:5432/earlypair?sslmode=disable",
			MaxConns:    16,
			MinConns:    2,
			MaxConnIdle: 5 * time.Minute,
			AutoMigrate: true,
		},
		Redis: Redis{URL: "redis://localhost:6379/0"},
		Log:   Log{Level: "info", Format: "text"},
		RPC: RPCPolicy{
			HedgeAfter:          400 * time.Millisecond,
			MaxHedges:           2,
			RequestTimeout:      12 * time.Second,
			BreakerFailures:     5,
			BreakerProbeAfter:   30 * time.Second,
			HeadLagEvictBlocks:  3,
			HeadLagEvictFor:     30 * time.Second,
			WSDropsBeforeDemote: 2,
			WSDropWindow:        10 * time.Minute,
			WSDemoteFor:         15 * time.Minute,
			WSBackoffMin:        500 * time.Millisecond,
			WSBackoffMax:        30 * time.Second,
		},
		Ingest: Ingest{
			MaxBackfillBlocks: 50_000,
			BackfillChunk:     200,
			DoorbellDebounce:  150 * time.Millisecond,
			ConfirmCheckEvery: 15 * time.Second,
			EnrichWorkers:     8,
			TrackWindow:       10 * time.Minute,
			TrackEvery:        3 * time.Second,
			Screen: Screen{
				After:           3 * time.Minute,
				MinLiquidityUSD: 10_000,
				MinVolumeUSD:    100,
				KeepIf:          "both",
			},
			EnrichQueueSize: 4096,
			PoolRetention:   48 * time.Hour,
		},
		Stream: Stream{
			ReplayWindow:    5 * time.Minute,
			ReplayMaxFrames: 20_000,
			ClientQueue:     512,
			PingInterval:    20 * time.Second,
			CoalesceWindow:  250 * time.Millisecond,
		},
		Auth: Auth{
			SessionTTL: 24 * time.Hour,
			NonceTTL:   5 * time.Minute,
			Domain:     "localhost",
		},
		Risk: RiskWeights{
			SellersThin:     14,
			RatioHigh:       20,
			LPUnlocked:      22,
			LPShortLock:     10,
			MintActive:      20,
			TaxNonZero:      8,
			TaxPunitive:     18,
			TopHeavyMild:    10,
			TopHeavy:        14,
			TopHeavySevere:  20,
			TopHeavyExtreme: 26,
			FewHolders:      8,
			Unverified:      6,
			// A sell that reverts in simulation means the position cannot be
			// exited at all. That single fact has to clear the gate threshold by
			// itself, regardless of how clean everything else about the pool looks.
			HoneypotSimFailed: 70,
			PoolFeeExtreme:    70,
			GateThreshold:     60,
		},
	}
}

// Load reads a YAML file over the defaults and applies env overrides. A missing
// file is not an error: the defaults plus env are a complete configuration.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(b, &cfg); err != nil {
				return cfg, fmt.Errorf("config: parse %s: %w", path, err)
			}
		case os.IsNotExist(err):
			// fall through to defaults + env
		default:
			return cfg, fmt.Errorf("config: read %s: %w", path, err)
		}
	}

	applyEnv(&cfg)
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("EARLYPAIR_ADDR"); v != "" {
		cfg.Server.Addr = v
	}
	if v := os.Getenv("EARLYPAIR_METRICS_ADDR"); v != "" {
		cfg.Server.MetricsAddr = v
	}
	if v := os.Getenv("DATABASE_URL"); v != "" {
		cfg.Database.URL = v
	}
	if v := os.Getenv("REDIS_URL"); v != "" {
		cfg.Redis.URL = v
	}
	if v := os.Getenv("EARLYPAIR_REDIS_DISABLED"); v != "" {
		cfg.Redis.Disabled = truthy(v)
	}
	if v := os.Getenv("EARLYPAIR_LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
	if v := os.Getenv("EARLYPAIR_LOG_FORMAT"); v != "" {
		cfg.Log.Format = v
	}
	if v := os.Getenv("EARLYPAIR_JWT_SECRET"); v != "" {
		cfg.Auth.JWTSecret = v
	}
	// The sign-in message names the site it is for, and wallets warn when that
	// does not match the page asking: a deployment must set its own host here.
	if v := os.Getenv("EARLYPAIR_AUTH_DOMAIN"); v != "" {
		cfg.Auth.Domain = v
	}
	if v := os.Getenv("EARLYPAIR_POOL_RETENTION"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			cfg.Ingest.PoolRetention = d
		}
	}
	if v := os.Getenv("EARLYPAIR_CORS_ORIGINS"); v != "" {
		cfg.Server.CORSOrigins = splitList(v)
	}
	if v := os.Getenv("EARLYPAIR_GATE_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Risk.GateThreshold = n
		}
	}
	// Per-endpoint URL overrides let an operator inject a keyed endpoint without
	// putting the key in a config file: EARLYPAIR_RPC_URL_<ID>, id upper-snaked.
	for i := range cfg.Endpoints {
		key := "EARLYPAIR_RPC_URL_" + envKey(cfg.Endpoints[i].ID)
		if v := os.Getenv(key); v != "" {
			cfg.Endpoints[i].URL = v
		}
	}
	// Explorer API keys are always env-supplied.
	for id, ex := range cfg.Explorers {
		if v := os.Getenv("EARLYPAIR_EXPLORER_KEY_" + envKey(id)); v != "" {
			ex.APIKey = v
			cfg.Explorers[id] = ex
		}
	}
}

func envKey(s string) string {
	s = strings.ToUpper(s)
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, ".", "_")
	return s
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Validate rejects configurations that would fail confusingly at runtime.
func (c Config) Validate() error {
	seenChain := map[chain.ID]bool{}
	for _, ch := range c.Chains {
		if ch.ID == "" {
			return fmt.Errorf("config: chain with empty id")
		}
		if seenChain[ch.ID] {
			return fmt.Errorf("config: duplicate chain %q", ch.ID)
		}
		seenChain[ch.ID] = true
		if ch.Kind == "" {
			return fmt.Errorf("config: chain %q has no kind", ch.ID)
		}
	}

	seenEndpoint := map[string]bool{}
	for _, e := range c.Endpoints {
		if e.ID == "" {
			return fmt.Errorf("config: endpoint with empty id (%s)", e.URL)
		}
		if seenEndpoint[e.ID] {
			return fmt.Errorf("config: duplicate endpoint id %q", e.ID)
		}
		seenEndpoint[e.ID] = true
		if !seenChain[e.ChainID] {
			return fmt.Errorf("config: endpoint %q references unknown chain %q", e.ID, e.ChainID)
		}
		switch e.Transport {
		case chain.HTTP, chain.WS:
		default:
			return fmt.Errorf("config: endpoint %q has invalid transport %q", e.ID, e.Transport)
		}
		if e.Transport == chain.WS && !strings.HasPrefix(e.URL, "ws") {
			return fmt.Errorf("config: endpoint %q is ws transport but url is %q", e.ID, MaskURL(e.URL))
		}
		if e.Transport == chain.HTTP && !strings.HasPrefix(e.URL, "http") {
			return fmt.Errorf("config: endpoint %q is http transport but url is %q", e.ID, MaskURL(e.URL))
		}
	}

	switch c.Ingest.Screen.KeepIf {
	case "", "either", "both":
	default:
		return fmt.Errorf("config: ingest.screen.keepIf must be either or both, not %q", c.Ingest.Screen.KeepIf)
	}
	if s := c.Ingest.Screen; s.After > 0 && c.Ingest.TrackWindow > 0 && s.After >= c.Ingest.TrackWindow {
		return fmt.Errorf("config: ingest.screen.after (%s) must be shorter than ingest.trackWindow (%s)",
			s.After, c.Ingest.TrackWindow)
	}

	// An enabled chain with no HTTP endpoint cannot ingest at all: polling is
	// the source of truth, so this is a hard error rather than a warning.
	for _, ch := range c.Chains {
		if !ch.Enabled {
			continue
		}
		hasHTTP := false
		for _, e := range c.Endpoints {
			if e.ChainID == ch.ID && e.Transport == chain.HTTP && !e.Disabled {
				hasHTTP = true
				break
			}
		}
		if !hasHTTP {
			return fmt.Errorf("config: chain %q is enabled but has no http endpoint; "+
				"polling is the source of truth and cannot run on websockets alone", ch.ID)
		}
	}
	return nil
}

// Registry builds the chain registry from the loaded config.
func (c Config) Registry() *chain.Registry { return chain.NewRegistry(c.Chains) }

// MaskURL hides credentials in an RPC URL. Every log line and API response uses
// this form — an API key that reaches a log is an API key that leaks.
func MaskURL(raw string) string {
	if raw == "" {
		return ""
	}
	s := raw
	// Strip userinfo (scheme://user:pass@host).
	if at := strings.Index(s, "@"); at > 0 {
		if slashes := strings.Index(s, "//"); slashes >= 0 && slashes < at {
			s = s[:slashes+2] + "****@" + s[at+1:]
		}
	}
	// Strip anything after a path segment that looks like a key: long opaque
	// segments, or anything following the common /v2/ /v3/ key prefixes.
	parts := strings.Split(s, "/")
	for i, p := range parts {
		if i < 3 { // scheme, "", host
			continue
		}
		if looksLikeSecret(p) {
			parts[i] = "••••"
		}
	}
	s = strings.Join(parts, "/")
	// Query-string keys.
	if q := strings.Index(s, "?"); q >= 0 {
		s = s[:q] + "?••••"
	}
	return s
}

func looksLikeSecret(seg string) bool {
	if len(seg) < 16 {
		return false
	}
	digits, letters := 0, 0
	for _, r := range seg {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			letters++
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return digits > 0 && letters > 0
}
