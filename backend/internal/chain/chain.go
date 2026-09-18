// Package chain holds the cross-chain domain model: chain descriptors, DEX
// factory registrations, and the pool/token entities the rest of the system
// passes around. Nothing in here talks to the network.
package chain

import (
	"fmt"
	"strings"
	"time"
)

// ID is the short chain identifier used in URLs and the UI ("eth", "base").
type ID string

// Kind selects the ingestion family for a chain.
type Kind string

const (
	KindEVM    Kind = "evm"
	KindSolana Kind = "solana"
	KindSUI    Kind = "sui"
)

// Transport is how an RPC endpoint is reached.
type Transport string

const (
	HTTP Transport = "http"
	WS   Transport = "ws"
)

// Model is the AMM shape of a pool. The frontend switches its entire liquidity
// panel on this value, so it must be set for every pool we emit.
type Model string

const (
	ModelV2     Model = "v2"     // constant product, full range, 50/50
	ModelCLMM   Model = "clmm"   // concentrated liquidity, user picks a range
	ModelStable Model = "stable" // stableswap curve
)

// State tracks how much we trust a pool's existence. Pools enter the feed at
// zero confirmations so the UI is fast, then settle.
type State string

const (
	StateUnconfirmed State = "unconfirmed"
	StateConfirmed   State = "confirmed"
	StateReorged     State = "reorged"
)

// EventKind identifies which creation event a factory emits, which in turn
// selects the decoder in events.go.
type EventKind string

const (
	EventPairCreatedV2      EventKind = "pair_created_v2"
	EventPoolCreatedV3      EventKind = "pool_created_v3"
	EventInitializeV4       EventKind = "initialize_v4"
	EventPoolCreatedSolidly EventKind = "pool_created_solidly"
	EventCurvePlainPool     EventKind = "curve_plain_pool"
)

// Factory is one DEX deployment we watch on a chain.
type Factory struct {
	Address string    `yaml:"address"`
	Dex     string    `yaml:"dex"`
	Model   Model     `yaml:"model"`
	Event   EventKind `yaml:"event"`
	// FeeBps is the fixed fee for models that do not encode it per pool (v2,
	// solidly volatile). Zero means "read it from the event or the pool".
	FeeBps int `yaml:"feeBps"`
}

// Chain is a full chain descriptor, loaded from config/chains.yaml.
type Chain struct {
	ID        ID     `yaml:"id"`
	Name      string `yaml:"name"`
	Tag       string `yaml:"tag"`
	Kind      Kind   `yaml:"kind"`
	NumericID uint64 `yaml:"numericId"`
	// BlockTime must be the chain's real block time. It converts wall-clock
	// windows into block counts — "the last hour of trades" is read as
	// 3600s / BlockTime blocks — so a stale value silently shrinks or stretches
	// every such window. The indexer measures it at startup and warns on drift.
	BlockTime time.Duration `yaml:"blockTime"`
	// PollInterval optionally paces the ingest loop independently of block
	// time, for chains whose blocks are faster than it is worth polling on a
	// public RPC budget. Unset, the loop polls every half block, clamped to
	// 200ms–1s.
	PollEvery time.Duration `yaml:"pollInterval"`
	// Confirmations is the depth at which we re-verify a pool and flip it to
	// confirmed. Chosen per chain: deep on chains that reorg, shallow on L2s.
	Confirmations uint64 `yaml:"confirmations"`
	// MaxLogRange is the starting getLogs chunk size. The poller shrinks this
	// per endpoint when a provider rejects the range, and remembers what worked.
	MaxLogRange uint64 `yaml:"maxLogRange"`
	// BackfillChunk overrides ingest.backfillChunk, the widest range a catch-up
	// asks for at once. The global value is a block count, and a block count is
	// a different span of time on every chain: 200 blocks is 40 minutes of
	// Ethereum but 20 seconds of Robinhood, where catching up that slowly never
	// catches up at all.
	BackfillChunk uint64 `yaml:"backfillChunk"`
	// StartBlock is where a cold cursor begins. Zero means "current head".
	StartBlock uint64 `yaml:"startBlock"`

	Multicall3 string `yaml:"multicall3"`
	// NativePriceRef is a deep, v2-style pool pairing the wrapped native token
	// with a stable. Reading its reserves is how the native price is derived —
	// the service carries no external price feed, so every number it shows is
	// traceable to a chain it already reads.
	NativePriceRef               string `yaml:"nativePriceRef"`
	NativePriceRefNative         string `yaml:"nativePriceRefNative"`
	NativePriceRefNativeDecimals int    `yaml:"nativePriceRefNativeDecimals"`
	NativePriceRefStableDecimals int    `yaml:"nativePriceRefStableDecimals"`
	// RouterV2 is the constant-product router used to build swap calldata.
	RouterV2 string `yaml:"routerV2"`
	// UniswapV4 names the contracts that quote and trade v4 pools. Unset, v4
	// pools on the chain cannot be quoted or traded.
	UniswapV4 *UniswapV4 `yaml:"uniswapV4"`
	// UniswapV3 names the contracts that quote and trade v3 pools. Unset, v3
	// pools on the chain are listed but not tradable.
	UniswapV3 *UniswapV3 `yaml:"uniswapV3"`
	// LPLockers are the locker contracts whose LP-token balance counts as a
	// locked position, plus any burn sinks.
	LPLockers    []string `yaml:"lpLockers"`
	Explorer     string   `yaml:"explorer"`
	NativeSymbol string   `yaml:"nativeSymbol"`
	// NativeIsStable marks a chain that pays gas in a dollar stablecoin, so the
	// native price is one dollar and needs no reference pool. Arc is Circle's
	// USDC-gas L1; the native balance and the ERC-20 USDC are the same money.
	NativeIsStable bool `yaml:"nativeIsStable"`
	// WrappedNative is the ERC-20 wrapper of the gas token, where one exists.
	// Arc has none: the gas token already has an ERC-20 interface, listed in
	// Stables, and pools pair against that address directly.
	WrappedNative string    `yaml:"wrappedNative"`
	Stables       []string  `yaml:"stables"`
	Factories     []Factory `yaml:"factories"`

	Enabled bool `yaml:"enabled"`
}

// UniswapV3 is a chain's Uniswap v3 trading deployment: the QuoterV2 that
// prices a swap by running it, and SwapRouter02 that executes it. Both are
// checked on-chain by calling factory() and comparing it with the configured
// v3 factory.
type UniswapV3 struct {
	Quoter     string `yaml:"quoter"`
	SwapRouter string `yaml:"swapRouter"`
	// Dex names the factory these contracts serve, matching the factory's dex
	// name in this chain's config. It matters because v3 forks are not
	// interchangeable: PancakeSwap v3 pools live in their own factory and their
	// own quoter, and pricing one through Uniswap's finds no pool at all.
	// Defaults to "Uniswap v3".
	Dex string `yaml:"dex"`
}

// DexName is the factory these v3 contracts serve.
func (u *UniswapV3) DexName() string {
	if u == nil || u.Dex == "" {
		return "Uniswap v3"
	}
	return u.Dex
}

// UniswapV4 is a chain's Uniswap v4 trading deployment.
type UniswapV4 struct {
	Quoter          string `yaml:"quoter"`
	UniversalRouter string `yaml:"universalRouter"`
	Permit2         string `yaml:"permit2"`
	// StateView reads a v4 pool's price, liquidity and ticks by pool id: a v4
	// pool has no contract of its own to ask. Unset, the depth chart is
	// unavailable on this chain — nothing else depends on it.
	StateView string `yaml:"stateView"`
	// ExtraSwapParam: the router's exact-input params carry one more uint256
	// between amountOutMinimum and hookData than the current Uniswap release —
	// Robinhood Chain calls it minHopPriceX36, Arc's router has it too. Encoded
	// without it, hookData is decoded from the wrong offset: a hookless pool
	// ignores the garbage and swaps anyway, a hooked pool reverts. That
	// asymmetry is what it looks like in the wild, so check both kinds before
	// concluding a chain does not need this.
	ExtraSwapParam bool `yaml:"extraSwapParam"`
}

// ExpectedBlockTime falls back to a sane default so a misconfigured chain does
// not produce a zero-interval poll loop that spins the CPU and the RPC budget.
func (c Chain) ExpectedBlockTime() time.Duration {
	if c.BlockTime <= 0 {
		return 12 * time.Second
	}
	return c.BlockTime
}

// PollInterval is the fallback timer for the ingest loop: half a block, capped
// at one second. The WebSocket doorbell normally fires well before this.
func (c Chain) PollInterval() time.Duration {
	if c.PollEvery > 0 {
		return c.PollEvery
	}
	half := c.ExpectedBlockTime() / 2
	if half > time.Second {
		return time.Second
	}
	if half < 200*time.Millisecond {
		return 200 * time.Millisecond
	}
	return half
}

// Token is a minimal token identity. Decimals is required before any amount
// coming from this token can be formatted or compared.
type Token struct {
	Address  string `json:"address"`
	Symbol   string `json:"symbol"`
	Name     string `json:"name,omitempty"`
	Decimals int    `json:"decimals"`
	// TotalSupply is a decimal string in base units (not scaled by decimals).
	TotalSupply string `json:"totalSupply,omitempty"`
}

// Pool is the entity the feed is built from. Nullable pointers are deliberate:
// enrichment fills them in progressively and the API must be able to say "not
// known yet" rather than inventing a number the user would trade on.
type Pool struct {
	ChainID ID     `json:"chainId"`
	Address string `json:"address"`
	Pair    string `json:"pair"`
	Dex     string `json:"dex"`
	Model   Model  `json:"model"`
	State   State  `json:"state"`

	CreatedAt    time.Time `json:"createdAt"`
	CreatedBlock uint64    `json:"createdBlock"`
	CreatedTx    string    `json:"createdTx"`
	LogIndex     uint      `json:"-"`
	// Backfilled marks pools discovered by a cold-start catch-up rather than
	// live, so the stream does not spam clients with hours-old pairs.
	Backfilled bool `json:"backfilled,omitempty"`

	Token0 *Token `json:"token0"`
	Token1 *Token `json:"token1"`

	FeeTierBps *int `json:"feeTierBps"`
	// Hooks is a Uniswap v4 pool's hook contract; empty when there is none.
	Hooks string `json:"hooks,omitempty"`

	LiquidityUSD *float64 `json:"liquidityUsd"`
	Volume1hUSD  *float64 `json:"volume1hUsd"`
	// VolAccel compares the last minute's volume with this pair's average
	// minute so far: 3.2 means the last minute traded 3.2x its usual pace.
	// Nil when the pair is too young, or too quiet, to compare.
	VolAccel      *float64 `json:"volAccel"`
	PriceChange1h *float64 `json:"priceChange1h"`
	Buyers1h      *int     `json:"buyers1h"`
	Sellers1h     *int     `json:"sellers1h"`
	// TradersPending counts swaps whose trader is not known yet. While it is
	// above zero, Buyers1h and Sellers1h are a floor, not a final count.
	TradersPending *int `json:"tradersPending"`

	Risk *RiskSummary `json:"risk"`

	// ScreenedAt is when the market tracker judged the pair against the feed's
	// liquidity and volume bar. RemovedAt is set when it failed: the pair is
	// gone from the feed and a removal frame was pushed.
	ScreenedAt *time.Time `json:"screenedAt,omitempty"`
	RemovedAt  *time.Time `json:"removedAt,omitempty"`
	// FavoritedAt is when the pair was starred. A favorite is tracked for as
	// long as it stays starred, and screening never removes it.
	FavoritedAt *time.Time `json:"favoritedAt"`
	// ImportedAt is when a user added this pool by address, for a pool the sweep
	// never saw because it was created before this service started watching.
	// The UI marks such a pair, since its figures start at the moment it was
	// added rather than at the pair's own launch.
	ImportedAt *time.Time `json:"importedAt,omitempty"`
}

// RiskSummary is the scorer's output as the UI consumes it.
type RiskSummary struct {
	Score   int         `json:"score"`
	Band    string      `json:"band"`
	Verdict string      `json:"verdict"`
	Checks  []RiskCheck `json:"checks"`
	// ScoredAt lets the UI show staleness rather than implying a fresh verdict.
	ScoredAt time.Time `json:"scoredAt"`
	// Complete is false while any establishable input is still pending, so a
	// half-probed pool is never mistaken for a cleared one.
	Complete bool `json:"complete"`
	// Unavailable names checks this deployment cannot perform at all, so the UI
	// can caveat a SAFE band honestly ("tax not simulated") rather than imply
	// every check ran.
	Unavailable []string `json:"unavailable,omitempty"`
}

// RiskCheck is one PASS/FAIL row in the fraud detection panel.
type RiskCheck struct {
	Label string `json:"label"`
	Value string `json:"value"`
	OK    bool   `json:"ok"`
	// Known distinguishes "checked, and it failed" from "not established yet".
	// Both render as not-OK, but only the first is evidence of anything, and
	// only the first contributes to the score.
	Known bool `json:"known"`
}

// Orientation says which side of a pool is the traded token and which is the
// quote it is priced in.
//
// A pool's token0 and token1 are ordered by address, which says nothing about
// economics: WETH sorts below plenty of new tokens and lands in slot 0. Anything
// that assumed slot 0 is the token being traded got three things backwards for
// those pools — the pair name ("WETH / NEWTOKEN"), which swaps count as buys
// (inverting the honeypot signal), and which direction a "buy" quote builds.
// Every one of those decisions goes through here instead.
type Orientation struct {
	Base  *Token // the token a user buys or sells
	Quote *Token // what it is priced in: a stable or the wrapped native
	// BaseIsToken0 maps the orientation back onto the pool's own slots, for
	// reserves and swap amounts, which are always reported by slot.
	BaseIsToken0 bool
}

// Orient decides a pool's orientation. The quote is whichever side is a
// configured stable, or failing that the wrapped native; a stable wins over the
// wrapped native, so WETH/USDC reads "WETH / USDC". When neither side is a known
// quote asset, slot order stands — there is no better information to use.
func Orient(c Chain, t0, t1 *Token) Orientation {
	rank := func(t *Token) int {
		if t == nil {
			return 0
		}
		for _, s := range c.Stables {
			if strings.EqualFold(s, t.Address) {
				return 2
			}
		}
		if IsNative(t.Address) || (c.WrappedNative != "" && strings.EqualFold(c.WrappedNative, t.Address)) {
			return 1
		}
		return 0
	}
	if rank(t0) > rank(t1) {
		return Orientation{Base: t1, Quote: t0, BaseIsToken0: false}
	}
	return Orientation{Base: t0, Quote: t1, BaseIsToken0: true}
}

// Name renders the pair the way traders read it: "PEPE / WETH".
func (o Orientation) Name() string { return PairName(o.Base, o.Quote) }

// PairName renders "AERO / USDC" from the two token symbols, tolerating tokens
// that have not been resolved yet.
func PairName(t0, t1 *Token) string {
	sym := func(t *Token) string {
		if t == nil || t.Symbol == "" {
			return "?"
		}
		return t.Symbol
	}
	return sym(t0) + " / " + sym(t1)
}

// Key is the stable identity used for dedupe and as the stream/cache key.
func (p Pool) Key() string { return string(p.ChainID) + ":" + strings.ToLower(p.Address) }

// Validate catches malformed pools before they reach the store or the stream.
func (p Pool) Validate() error {
	if p.ChainID == "" {
		return fmt.Errorf("pool: empty chain id")
	}
	if p.Address == "" {
		return fmt.Errorf("pool: empty address")
	}
	// A pool found by the sweep always carries the log that created it. One
	// added by address may not: the creation log can be older than any state
	// public endpoints keep, and refusing the pool for that would mean refusing
	// to trade a pair that plainly exists.
	if p.CreatedTx == "" && p.ImportedAt == nil {
		return fmt.Errorf("pool %s: empty creating tx", p.Address)
	}
	switch p.Model {
	case ModelV2, ModelCLMM, ModelStable:
	default:
		return fmt.Errorf("pool %s: unknown amm model %q", p.Address, p.Model)
	}
	return nil
}

// Registry indexes configured chains by ID.
type Registry struct {
	chains map[ID]Chain
	order  []ID
}

func NewRegistry(chains []Chain) *Registry {
	r := &Registry{chains: make(map[ID]Chain, len(chains))}
	for _, c := range chains {
		if !c.Enabled {
			continue
		}
		r.chains[c.ID] = c
		r.order = append(r.order, c.ID)
	}
	return r
}

func (r *Registry) Get(id ID) (Chain, bool) { c, ok := r.chains[id]; return c, ok }
func (r *Registry) IDs() []ID               { return append([]ID(nil), r.order...) }

func (r *Registry) All() []Chain {
	out := make([]Chain, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.chains[id])
	}
	return out
}
