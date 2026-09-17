package chain

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// TestKnownTopicHashes pins the derived topics against values that can be
// checked independently on any block explorer.
//
// The topics are computed with keccak at init rather than pasted in, which is
// safer — a typo in a pasted constant matches nothing and produces an empty
// feed, the hardest failure in this system to notice. This test is the other
// half of that trade: it catches a wrong *signature*, which would fail just as
// silently.
func TestKnownTopicHashes(t *testing.T) {
	cases := []struct {
		name string
		got  common.Hash
		want string
	}{
		{
			name: "UniswapV2 PairCreated",
			got:  TopicPairCreatedV2,
			want: "0x0d3648bd0f6ba80134a33ba9275ac585d9d315f0ad8355cddefde31afa28d0e9",
		},
		{
			name: "UniswapV3 PoolCreated",
			got:  TopicPoolCreatedV3,
			want: "0x783cca1c0412dd0d695e784568c96da2e9c22ff989357a2e8b1d9b2b4e6b7118",
		},
		{
			name: "ERC20 Transfer",
			got:  TopicTransfer,
			want: "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",
		},
		{
			name: "OwnershipTransferred",
			got:  TopicForSignature(SigOwnershipTransferred),
			want: "0x8be0079c531659141344cd1fd0a4f28419497f9722a3daafe3b4186f6b6457e0",
		},
	}
	for _, c := range cases {
		if !strings.EqualFold(c.got.Hex(), c.want) {
			t.Errorf("%s topic0 = %s, want %s", c.name, c.got.Hex(), c.want)
		}
	}
}

func TestNormalizeSignature(t *testing.T) {
	cases := map[string]string{
		"PairCreated(address,address,address,uint256)":                                               "PairCreated(address,address,address,uint256)",
		"event PairCreated(address indexed token0, address indexed token1, address pair, uint256 n)": "PairCreated(address,address,address,uint256)",
		"  Transfer( address from , address to , uint256 value )":                                    "Transfer(address,address,uint256)",
	}
	for in, want := range cases {
		if got := NormalizeSignature(in); got != want {
			t.Errorf("NormalizeSignature(%q) = %q, want %q", in, got, want)
		}
	}

	// A signature pasted straight out of Solidity must hash to the same topic
	// as its canonical form, since that is what users will actually paste.
	verbose := "event PairCreated(address indexed token0, address indexed token1, address pair, uint256 n)"
	if TopicForSignature(verbose) != TopicPairCreatedV2 {
		t.Error("a Solidity-style signature did not hash to the canonical topic")
	}
}

func TestDecodeCreationV2(t *testing.T) {
	factory := "0x5C69bEe701ef814a2B6a3EDD4B1652CB9cc5aA6f"
	pool := "0x0d4a11d5EEaaC28EC3F61d100daF4d40471f1852"
	token0 := "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"
	token1 := "0xdAC17F958D2ee523a2206206994597C13D831ec7"

	data := make([]byte, 64)
	copy(data[12:32], common.HexToAddress(pool).Bytes())
	data[63] = 1

	log := types.Log{
		Address: common.HexToAddress(factory),
		Topics: []common.Hash{
			TopicPairCreatedV2,
			common.BytesToHash(common.HexToAddress(token0).Bytes()),
			common.BytesToHash(common.HexToAddress(token1).Bytes()),
		},
		Data:   data,
		TxHash: common.HexToHash("0xabc"),
	}

	idx := NewFactoryIndex([]Factory{{
		Address: factory, Dex: "Uniswap v2", Model: ModelV2, Event: EventPairCreatedV2,
	}})

	got, err := DecodeCreation(log, idx)
	if err != nil {
		t.Fatalf("DecodeCreation: %v", err)
	}
	if !strings.EqualFold(got.PoolAddress, pool) {
		t.Errorf("pool = %s, want %s", got.PoolAddress, pool)
	}
	if !strings.EqualFold(got.Token0, token0) || !strings.EqualFold(got.Token1, token1) {
		t.Errorf("tokens = %s / %s, want %s / %s", got.Token0, got.Token1, token0, token1)
	}
	if got.Model != ModelV2 {
		t.Errorf("model = %s, want %s", got.Model, ModelV2)
	}
	if got.FeeBps == nil || *got.FeeBps != 30 {
		t.Errorf("feeBps = %v, want 30", got.FeeBps)
	}
}

func TestDecodeCreationRejectsUnknownFactory(t *testing.T) {
	// The same topic emitted by a contract we did not register is noise. It
	// must be skipped rather than guessed at: a pool whose DEX we cannot name
	// is a pool we cannot quote, price or exit.
	log := types.Log{
		Address: common.HexToAddress("0x000000000000000000000000000000000000bEEF"),
		Topics: []common.Hash{
			TopicPairCreatedV2,
			common.BytesToHash(common.HexToAddress("0x1").Bytes()),
			common.BytesToHash(common.HexToAddress("0x2").Bytes()),
		},
		Data: make([]byte, 64),
	}
	idx := NewFactoryIndex([]Factory{{
		Address: "0x5C69bEe701ef814a2B6a3EDD4B1652CB9cc5aA6f",
		Dex:     "Uniswap v2", Model: ModelV2, Event: EventPairCreatedV2,
	}})
	if _, err := DecodeCreation(log, idx); err == nil {
		t.Error("a log from an unregistered factory was accepted")
	}
}

func TestDecodeCreationSurvivesTruncatedLogs(t *testing.T) {
	// A flaky provider can return a short or malformed log. Skipping it must
	// never panic, because that would take the whole chain's ingester down.
	idx := NewFactoryIndex([]Factory{{
		Address: "0x5C69bEe701ef814a2B6a3EDD4B1652CB9cc5aA6f",
		Dex:     "Uniswap v2", Model: ModelV2, Event: EventPairCreatedV2,
	}})
	for _, data := range [][]byte{nil, {}, make([]byte, 5), make([]byte, 31)} {
		log := types.Log{
			Address: common.HexToAddress("0x5C69bEe701ef814a2B6a3EDD4B1652CB9cc5aA6f"),
			Topics:  []common.Hash{TopicPairCreatedV2},
			Data:    data,
		}
		if _, err := DecodeCreation(log, idx); err == nil {
			t.Errorf("a truncated log (%d bytes) decoded without error", len(data))
		}
	}
}

func TestDecodeSwapSigns(t *testing.T) {
	pool := common.HexToAddress("0x0d4a11d5EEaaC28EC3F61d100daF4d40471f1852")
	trader := common.HexToAddress("0x00000000000000000000000000000000000000aa")

	// v2: 100 token1 in, 50 token0 out — a buy of token0.
	data := make([]byte, 128)
	big.NewInt(0).FillBytes(data[0:32])    // amount0In
	big.NewInt(100).FillBytes(data[32:64]) // amount1In
	big.NewInt(50).FillBytes(data[64:96])  // amount0Out
	big.NewInt(0).FillBytes(data[96:128])  // amount1Out

	sw, err := DecodeSwap(types.Log{
		Address: pool,
		Topics: []common.Hash{
			TopicSwapV2,
			common.BytesToHash(trader.Bytes()),
			common.BytesToHash(trader.Bytes()),
		},
		Data: data,
	})
	if err != nil {
		t.Fatalf("DecodeSwap: %v", err)
	}
	if !sw.Buyer {
		t.Error("a swap that paid out token0 was not classified as a buy")
	}
	if sw.Amount0.Sign() >= 0 {
		t.Errorf("amount0 = %s; the pool paid token0 out, so it must be negative", sw.Amount0)
	}
	if !strings.EqualFold(sw.Trader, trader.Hex()) {
		t.Errorf("trader = %s, want %s", sw.Trader, trader.Hex())
	}
}

// v4Initialize builds an Initialize log the way the PoolManager emits it.
func v4Initialize(poolID common.Hash, c0, c1 string, fee uint64, hooks string) types.Log {
	data := make([]byte, 160)
	new(big.Int).SetUint64(fee).FillBytes(data[0:32])
	big.NewInt(60).FillBytes(data[32:64])
	copy(data[76:96], common.HexToAddress(hooks).Bytes())
	return types.Log{
		Address: common.HexToAddress("0x498581fF718922c3f8e6A244956aF099B2652b2b"),
		Topics: []common.Hash{
			TopicInitializeV4, poolID,
			common.BytesToHash(common.HexToAddress(c0).Bytes()),
			common.BytesToHash(common.HexToAddress(c1).Bytes()),
		},
		Data:   data,
		TxHash: common.HexToHash("0xabc"),
	}
}

var v4Index = NewFactoryIndex([]Factory{{
	Address: "0x498581fF718922c3f8e6A244956aF099B2652b2b",
	Dex:     "Uniswap v4", Model: ModelCLMM, Event: EventInitializeV4,
}})

// TestDecodeV4NativeDynamicFeeHooked mirrors the most common new pool seen live
// on Base: native ETH as currency0, a dynamic fee, and a custom hook.
func TestDecodeV4NativeDynamicFeeHooked(t *testing.T) {
	id := common.HexToHash("0x5dee8e5a400011f4000000000000000000000000000000000000000000000001")
	hook := "0x00000000000000000000000000000000DeaDbeC0"
	token := "0x908F9842e9D9A0a297a052c2D76Fcea13EcD2d15"

	c, err := DecodeCreation(v4Initialize(id, NativeAddress, token, 0x800000, hook), v4Index)
	if err != nil {
		t.Fatalf("DecodeCreation: %v", err)
	}
	if !IsV4PoolID(c.PoolAddress) || !strings.EqualFold(c.PoolAddress, id.Hex()) {
		t.Errorf("pool id = %q, want the PoolId %s", c.PoolAddress, id.Hex())
	}
	if !IsNative(c.Token0) {
		t.Errorf("token0 = %s, want the native sentinel", c.Token0)
	}
	if c.FeeBps != nil {
		t.Errorf("dynamic-fee pool reported a fee tier of %d bps; the flag is not a fee", *c.FeeBps)
	}
	if !strings.EqualFold(c.Hooks, hook) {
		t.Errorf("hooks = %q, want %s", c.Hooks, hook)
	}
}

func TestDecodeV4StaticFeeNoHooks(t *testing.T) {
	id := common.HexToHash("0x01")
	c, err := DecodeCreation(v4Initialize(id, "0x0000000000000000000000000000000000000001",
		"0x0000000000000000000000000000000000000002", 3000, NativeAddress), v4Index)
	if err != nil {
		t.Fatalf("DecodeCreation: %v", err)
	}
	if c.FeeBps == nil || *c.FeeBps != 30 {
		t.Errorf("fee = %v, want 30 bps", c.FeeBps)
	}
	if c.Hooks != "" {
		t.Errorf("a pool with no hook reported hooks=%q", c.Hooks)
	}
}

// TestDecodeSwapV4Sign pins the convention verified against live Base swaps:
// v4 reports the swapper's side, so a positive amount0 is the swapper receiving
// token0 — a buy — and the decoded amounts come back pool-side like v2/v3.
func TestDecodeSwapV4Sign(t *testing.T) {
	id := common.HexToHash("0xac76")
	router := common.HexToAddress("0x6fF5693b99212Da76ad316178A184AB56D299b43")

	data := make([]byte, 256)
	big.NewInt(500).FillBytes(data[0:32]) // swapper receives 500 token0
	neg := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(2))
	neg.FillBytes(data[32:64]) // swapper pays 2 token1

	sw, err := DecodeSwapV4(types.Log{
		Topics: []common.Hash{TopicSwapV4, id, common.BytesToHash(router.Bytes())},
		Data:   data,
	})
	if err != nil {
		t.Fatalf("DecodeSwapV4: %v", err)
	}
	if !sw.Buyer {
		t.Error("a swap where the swapper received token0 was not a buy")
	}
	if sw.Amount0.Cmp(big.NewInt(-500)) != 0 || sw.Amount1.Cmp(big.NewInt(2)) != 0 {
		t.Errorf("amounts = %s / %s, want pool-side -500 / 2", sw.Amount0, sw.Amount1)
	}
	if !strings.EqualFold(sw.Pool, id.Hex()) {
		t.Errorf("pool = %s, want the PoolId from topic1", sw.Pool)
	}
}

func TestNativeIsAQuoteAsset(t *testing.T) {
	c := Chain{WrappedNative: "0x4200000000000000000000000000000000000006"}
	native := &Token{Address: NativeAddress, Symbol: "ETH"}
	token := &Token{Address: "0x908f9842e9d9a0a297a052c2d76fcea13ecd2d15", Symbol: "PEPE"}

	o := Orient(c, native, token)
	if o.Name() != "PEPE / ETH" || o.BaseIsToken0 {
		t.Errorf("native ETH in token0 oriented as %q (baseIsToken0=%v), want %q",
			o.Name(), o.BaseIsToken0, "PEPE / ETH")
	}
}

// TestPollIntervalIsIndependentOfBlockTime: block time must be the chain's real
// value because it sizes wall-clock windows, while polling can be paced slower
// to spare a public RPC budget. Before the two were split, keeping the poll
// pace meant lying about block time, which shrank "last hour" windows to
// minutes on BSC, Arbitrum and Robinhood.
func TestPollIntervalIsIndependentOfBlockTime(t *testing.T) {
	fast := Chain{BlockTime: 100 * time.Millisecond}
	if got := fast.PollInterval(); got != 200*time.Millisecond {
		t.Errorf("unpaced 100ms chain polls every %s, want the 200ms floor", got)
	}
	paced := Chain{BlockTime: 100 * time.Millisecond, PollEvery: time.Second}
	if got := paced.PollInterval(); got != time.Second {
		t.Errorf("paced chain polls every %s, want 1s", got)
	}
	if got := paced.ExpectedBlockTime(); got != 100*time.Millisecond {
		t.Errorf("pacing changed the block time to %s; it must stay the real 100ms", got)
	}
}
