package chain

import (
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// Event signatures. Topic hashes are derived with keccak at init rather than
// hardcoded: a mistyped constant would silently match nothing, and "the feed is
// empty" is the hardest failure mode to notice in this system.
const (
	SigPairCreatedV2 = "PairCreated(address,address,address,uint256)"
	SigPoolCreatedV3 = "PoolCreated(address,address,uint24,int24,address)"
	// Uniswap v4's deployed PoolManager event. The eight-argument form is the
	// canonical one on-chain; the six-argument form that circulated in drafts
	// never shipped, so matching on it would yield no pools.
	SigInitializeV4       = "Initialize(bytes32,address,address,uint24,int24,address,uint160,int24)"
	SigPoolCreatedSolidly = "PoolCreated(address,address,bool,address,uint256)"

	SigSwapV2 = "Swap(address,uint256,uint256,uint256,uint256,address)"
	SigSwapV3 = "Swap(address,address,int256,int256,uint160,uint128,int24)"
	// Emitted by the v4 PoolManager singleton for every pool, keyed by PoolId.
	SigSwapV4 = "Swap(bytes32,address,int128,int128,uint160,uint128,int24,uint24)"
	SigSyncV2 = "Sync(uint112,uint112)"
	// Each of these was confirmed against real logs from recently created pools
	// before being added: the decoders are written against what chains emit.
	// Aerodrome (Solidly-style) pools: both indexed addresses first, and uint256
	// reserves, so their topics differ from Uniswap v2's.
	SigSwapSolidly = "Swap(address,address,uint256,uint256,uint256,uint256)"
	SigSyncSolidly = "Sync(uint256,uint256)"
	// PancakeSwap v3 appends two protocol-fee fields to the v3 Swap event.
	SigSwapPancakeV3 = "Swap(address,address,int256,int256,uint160,uint128,int24,uint128,uint128)"
	// Emitted by a v3 pool when its opening price is set.
	SigInitializeV3 = "Initialize(uint160,int24)"
	// Every liquidity change on a v4 pool, emitted by the PoolManager.
	SigModifyLiquidityV4    = "ModifyLiquidity(bytes32,address,int24,int24,int256,bytes32)"
	SigTransfer             = "Transfer(address,address,uint256)"
	SigOwnershipTransferred = "OwnershipTransferred(address,address)"
)

// Topic hashes, computed once at package init.
var (
	TopicPairCreatedV2      = topic(SigPairCreatedV2)
	TopicPoolCreatedV3      = topic(SigPoolCreatedV3)
	TopicInitializeV4       = topic(SigInitializeV4)
	TopicPoolCreatedSolidly = topic(SigPoolCreatedSolidly)

	TopicSwapV2 = topic(SigSwapV2)
	TopicSwapV3 = topic(SigSwapV3)
	TopicSwapV4 = topic(SigSwapV4)
	TopicSyncV2 = topic(SigSyncV2)

	TopicSwapSolidly     = topic(SigSwapSolidly)
	TopicSyncSolidly     = topic(SigSyncSolidly)
	TopicSwapPancakeV3   = topic(SigSwapPancakeV3)
	TopicInitializeV3    = topic(SigInitializeV3)
	TopicModifyLiquidity = topic(SigModifyLiquidityV4)
	TopicTransfer        = topic(SigTransfer)
)

func topic(sig string) common.Hash { return crypto.Keccak256Hash([]byte(sig)) }

// TopicForSignature hashes a human-readable event signature, after normalising
// whitespace. Used by the topic watchlist, where users paste signatures by hand.
func TopicForSignature(sig string) common.Hash {
	return crypto.Keccak256Hash([]byte(NormalizeSignature(sig)))
}

// NormalizeSignature strips the spaces, argument names and "event " prefix that
// people paste in from Solidity source, leaving the canonical form that hashes
// to the on-chain topic0.
func NormalizeSignature(sig string) string {
	s := strings.TrimSpace(sig)
	s = strings.TrimPrefix(s, "event ")
	open := strings.Index(s, "(")
	close := strings.LastIndex(s, ")")
	if open < 0 || close < open {
		return strings.ReplaceAll(s, " ", "")
	}
	name := strings.TrimSpace(s[:open])
	args := s[open+1 : close]
	var parts []string
	for _, a := range strings.Split(args, ",") {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		// "address indexed token0" -> "address"
		fields := strings.Fields(a)
		parts = append(parts, fields[0])
	}
	return name + "(" + strings.Join(parts, ",") + ")"
}

// ErrUnsupportedEvent is returned for factory kinds whose creation event does
// not carry the pool address and therefore needs receipt-level resolution we do
// not implement yet (Curve's factories). Callers skip rather than guess.
var ErrUnsupportedEvent = errors.New("chain: creation event needs receipt resolution")

// CreationTopics returns the topic0 set to filter on for a chain's factories,
// deduplicated. An empty result means the chain has nothing to watch.
func CreationTopics(factories []Factory) []common.Hash {
	seen := map[common.Hash]bool{}
	var out []common.Hash
	for _, f := range factories {
		h, ok := creationTopic(f.Event)
		if !ok || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

func creationTopic(k EventKind) (common.Hash, bool) {
	switch k {
	case EventPairCreatedV2:
		return TopicPairCreatedV2, true
	case EventPoolCreatedV3:
		return TopicPoolCreatedV3, true
	case EventInitializeV4:
		return TopicInitializeV4, true
	case EventPoolCreatedSolidly:
		return TopicPoolCreatedSolidly, true
	default:
		return common.Hash{}, false
	}
}

// FactoryIndex maps a lowercased factory address to its registration so a log
// can be attributed to the right DEX and AMM model in O(1).
type FactoryIndex map[string]Factory

func NewFactoryIndex(factories []Factory) FactoryIndex {
	idx := make(FactoryIndex, len(factories))
	for _, f := range factories {
		idx[strings.ToLower(f.Address)] = f
	}
	return idx
}

func (i FactoryIndex) Addresses() []common.Address {
	out := make([]common.Address, 0, len(i))
	for a := range i {
		out = append(out, common.HexToAddress(a))
	}
	return out
}

// Creation is the decoded result of a pool-creation log.
type Creation struct {
	PoolAddress string
	Token0      string
	Token1      string
	Dex         string
	Model       Model
	FeeBps      *int
	// Hooks is a Uniswap v4 pool's hook contract, empty when it has none. A
	// hook runs arbitrary code on every swap — it can take fees or refuse sells
	// — so it is carried through to the risk summary rather than dropped.
	Hooks string
}

// NativeAddress is how Uniswap v4 represents a chain's native currency (ETH,
// BNB) as a pool token: the zero address. It has no contract to call.
const NativeAddress = "0x0000000000000000000000000000000000000000"

// IsNative reports whether a token address is the native-currency sentinel.
func IsNative(address string) bool { return strings.EqualFold(address, NativeAddress) }

// IsV4PoolID reports whether a pool identifier is a v4 PoolId (bytes32) rather
// than a pool contract address. v4 pools have no address of their own.
func IsV4PoolID(id string) bool { return len(id) == 66 && strings.HasPrefix(id, "0x") }

// dynamicFeeFlag marks a v4 pool whose fee is set per swap by its hook.
const dynamicFeeFlag = 0x800000

// DecodeCreation turns a raw creation log into a Creation, using the factory
// registration to attribute the DEX and AMM model. It never panics on short
// data — malformed logs from a flaky provider must be skipped, not fatal.
func DecodeCreation(log types.Log, idx FactoryIndex) (Creation, error) {
	f, ok := idx[strings.ToLower(log.Address.Hex())]
	if !ok {
		return Creation{}, fmt.Errorf("chain: log from unregistered factory %s", log.Address.Hex())
	}
	c := Creation{Dex: f.Dex, Model: f.Model}
	if f.FeeBps > 0 {
		fee := f.FeeBps
		c.FeeBps = &fee
	}

	switch f.Event {
	case EventPairCreatedV2:
		if len(log.Topics) < 3 || len(log.Data) < 32 {
			return Creation{}, fmt.Errorf("chain: short PairCreated log in %s", log.TxHash.Hex())
		}
		c.Token0 = addrFromTopic(log.Topics[1])
		c.Token1 = addrFromTopic(log.Topics[2])
		c.PoolAddress = addrFromWord(log.Data[0:32])
		if c.FeeBps == nil {
			fee := 30 // 0.30% is the universal v2 fork default
			c.FeeBps = &fee
		}

	case EventPoolCreatedV3:
		if len(log.Topics) < 4 || len(log.Data) < 64 {
			return Creation{}, fmt.Errorf("chain: short PoolCreated log in %s", log.TxHash.Hex())
		}
		c.Token0 = addrFromTopic(log.Topics[1])
		c.Token1 = addrFromTopic(log.Topics[2])
		// fee is a uint24 in hundredths of a bip: 3000 == 0.30% == 30 bps.
		feeRaw := new(big.Int).SetBytes(log.Topics[3].Bytes())
		fee := int(feeRaw.Int64() / 100)
		c.FeeBps = &fee
		c.PoolAddress = addrFromWord(log.Data[32:64])

	case EventInitializeV4:
		// Data: fee, tickSpacing, hooks, sqrtPriceX96, tick — one word each.
		if len(log.Topics) < 4 || len(log.Data) < 96 {
			return Creation{}, fmt.Errorf("chain: short Initialize log in %s", log.TxHash.Hex())
		}
		// v4 pools are singleton-managed and identified by a bytes32 PoolId, not
		// an address. We carry the id in the address slot; the API documents it.
		c.PoolAddress = strings.ToLower(log.Topics[1].Hex())
		// currency0 may be the zero address: native ETH/BNB, no wrapper.
		c.Token0 = addrFromTopic(log.Topics[2])
		c.Token1 = addrFromTopic(log.Topics[3])
		feeRaw := new(big.Int).SetBytes(log.Data[0:32]).Uint64()
		if feeRaw&dynamicFeeFlag == 0 {
			// Hundredths of a bip: 3000 == 0.30% == 30 bps.
			fee := int(feeRaw / 100)
			c.FeeBps = &fee
		}
		// A dynamic-fee pool's fee is whatever its hook decides on each swap.
		// Reporting the flag value as a fee tier would be a made-up number, so
		// the tier stays unknown.
		if hooks := addrFromWord(log.Data[64:96]); !IsNative(hooks) {
			c.Hooks = hooks
		}

	case EventPoolCreatedSolidly:
		if len(log.Topics) < 4 || len(log.Data) < 32 {
			return Creation{}, fmt.Errorf("chain: short solidly PoolCreated log in %s", log.TxHash.Hex())
		}
		c.Token0 = addrFromTopic(log.Topics[1])
		c.Token1 = addrFromTopic(log.Topics[2])
		c.PoolAddress = addrFromWord(log.Data[0:32])
		// The third indexed arg is the stable flag, which picks the curve.
		if log.Topics[3].Big().Sign() != 0 {
			c.Model = ModelStable
			if c.FeeBps == nil {
				fee := 5
				c.FeeBps = &fee
			}
		} else {
			c.Model = ModelV2
			if c.FeeBps == nil {
				fee := 30
				c.FeeBps = &fee
			}
		}

	default:
		return Creation{}, ErrUnsupportedEvent
	}

	if c.PoolAddress == "" || c.Token0 == "" || c.Token1 == "" {
		return Creation{}, fmt.Errorf("chain: incomplete creation decode in %s", log.TxHash.Hex())
	}
	return c, nil
}

// Swap is a decoded trade, normalised across AMM generations. Amounts are
// signed from the pool's perspective: positive means the pool received.
type Swap struct {
	Pool    string
	Trader  string
	Amount0 *big.Int
	Amount1 *big.Int
	// Buyer is true when the trader received token0 (paid token1). The unique
	// buyer/seller address counts that drive the honeypot signal come from this.
	Buyer bool
	// SqrtPriceX96 is the pool price after the swap, for concentrated-liquidity
	// pools (v3, PancakeSwap v3, v4). Nil for v2-style pools, whose price comes
	// from the Sync event that accompanies every swap.
	SqrtPriceX96 *big.Int
}

// DecodeSwapV4 reads a PoolManager Swap event. Pool is the PoolId, and Trader
// is the event's sender — which is the router, not the person trading, so the
// caller must replace it with the transaction's signer before counting
// addresses.
//
// Sign convention, verified against 341 live Base swaps before this was
// written: v4 reports amounts from the *swapper's* side (positive means the
// swapper received), the opposite of v3. Every one of those swaps with a
// positive amount0 moved the price of token0 up. The amounts are negated here
// so Swap keeps one convention across all generations — pool-side.
func DecodeSwapV4(log types.Log) (Swap, error) {
	if len(log.Topics) < 3 || log.Topics[0] != TopicSwapV4 || len(log.Data) < 64 {
		return Swap{}, errors.New("chain: not a v4 swap log")
	}
	a0 := signedWord(log.Data[0:32])
	a1 := signedWord(log.Data[32:64])
	sw := Swap{
		Pool:    strings.ToLower(log.Topics[1].Hex()),
		Trader:  addrFromTopic(log.Topics[2]),
		Amount0: new(big.Int).Neg(a0),
		Amount1: new(big.Int).Neg(a1),
		Buyer:   a0.Sign() > 0, // the swapper received token0
	}
	if len(log.Data) >= 96 {
		sw.SqrtPriceX96 = new(big.Int).SetBytes(log.Data[64:96])
	}
	return sw, nil
}

// DecodeSwap handles both v2-style and v3-style Swap events.
func DecodeSwap(log types.Log) (Swap, error) {
	if len(log.Topics) == 0 {
		return Swap{}, errors.New("chain: swap log with no topics")
	}
	s := Swap{Pool: strings.ToLower(log.Address.Hex())}

	switch log.Topics[0] {
	case TopicSwapV2, TopicSwapSolidly:
		// Same data layout in both; only the argument order of the two indexed
		// addresses differs, and topic 2 is the recipient either way.
		if len(log.Topics) < 3 || len(log.Data) < 128 {
			return Swap{}, errors.New("chain: short v2 swap log")
		}
		// The "to" address is the recipient of the output; sender is usually a
		// router, so "to" is the closer proxy for a distinct human trader.
		s.Trader = addrFromTopic(log.Topics[2])
		a0In := new(big.Int).SetBytes(log.Data[0:32])
		a1In := new(big.Int).SetBytes(log.Data[32:64])
		a0Out := new(big.Int).SetBytes(log.Data[64:96])
		a1Out := new(big.Int).SetBytes(log.Data[96:128])
		s.Amount0 = new(big.Int).Sub(a0In, a0Out)
		s.Amount1 = new(big.Int).Sub(a1In, a1Out)
		s.Buyer = a0Out.Sign() > 0

	case TopicSwapV3, TopicSwapPancakeV3:
		// PancakeSwap v3 only appends fields, so the shared prefix decodes alike.
		if len(log.Topics) < 3 || len(log.Data) < 96 {
			return Swap{}, errors.New("chain: short v3 swap log")
		}
		s.Trader = addrFromTopic(log.Topics[2]) // recipient
		s.Amount0 = signedWord(log.Data[0:32])
		s.Amount1 = signedWord(log.Data[32:64])
		s.SqrtPriceX96 = new(big.Int).SetBytes(log.Data[64:96])
		// Negative amount0 means the pool paid out token0, i.e. a buy.
		s.Buyer = s.Amount0.Sign() < 0

	default:
		return Swap{}, errors.New("chain: not a recognised swap event")
	}
	return s, nil
}

// Reserves is a decoded v2 Sync event, the cheapest liquidity source there is:
// it arrives in the same log sweep as the swaps, costing no extra RPC call.
type Reserves struct {
	Pool     string
	Reserve0 *big.Int
	Reserve1 *big.Int
}

func DecodeSync(log types.Log) (Reserves, error) {
	if len(log.Topics) > 0 && log.Topics[0] != TopicSyncV2 && log.Topics[0] != TopicSyncSolidly {
		return Reserves{}, errors.New("chain: not a sync log")
	}
	if len(log.Data) < 64 {
		return Reserves{}, errors.New("chain: short sync log")
	}
	return Reserves{
		Pool:     strings.ToLower(log.Address.Hex()),
		Reserve0: new(big.Int).SetBytes(log.Data[0:32]),
		Reserve1: new(big.Int).SetBytes(log.Data[32:64]),
	}, nil
}

// DecodeInitializeV3 reads a v3 pool's opening price.
func DecodeInitializeV3(log types.Log) (*big.Int, error) {
	if len(log.Topics) == 0 || log.Topics[0] != TopicInitializeV3 || len(log.Data) < 32 {
		return nil, errors.New("chain: not a v3 initialize log")
	}
	return new(big.Int).SetBytes(log.Data[0:32]), nil
}

// DecodeInitializeV4Price reads a v4 pool's opening price from its Initialize
// event (data: fee, tickSpacing, hooks, sqrtPriceX96, tick).
func DecodeInitializeV4Price(log types.Log) (*big.Int, error) {
	if len(log.Topics) < 2 || log.Topics[0] != TopicInitializeV4 || len(log.Data) < 128 {
		return nil, errors.New("chain: not a v4 initialize log")
	}
	return new(big.Int).SetBytes(log.Data[96:128]), nil
}

// LiquidityChange is a decoded v4 ModifyLiquidity event.
type LiquidityChange struct {
	PoolID         string
	TickLower      int32
	TickUpper      int32
	LiquidityDelta *big.Int
}

// DecodeModifyLiquidityV4 reads one v4 liquidity change. Data: tickLower,
// tickUpper, liquidityDelta, salt.
func DecodeModifyLiquidityV4(log types.Log) (LiquidityChange, error) {
	if len(log.Topics) < 2 || log.Topics[0] != TopicModifyLiquidity || len(log.Data) < 96 {
		return LiquidityChange{}, errors.New("chain: not a v4 modify-liquidity log")
	}
	return LiquidityChange{
		PoolID:         strings.ToLower(log.Topics[1].Hex()),
		TickLower:      int32(signedWord(log.Data[0:32]).Int64()),
		TickUpper:      int32(signedWord(log.Data[32:64]).Int64()),
		LiquidityDelta: signedWord(log.Data[64:96]),
	}, nil
}

func addrFromTopic(h common.Hash) string {
	return strings.ToLower(common.BytesToAddress(h.Bytes()).Hex())
}

func addrFromWord(w []byte) string {
	if len(w) < 32 {
		return ""
	}
	return strings.ToLower(common.BytesToAddress(w).Hex())
}

// signedWord reads a two's-complement int256 out of a 32-byte word.
func signedWord(w []byte) *big.Int {
	v := new(big.Int).SetBytes(w)
	// If the top bit is set the value is negative: subtract 2^256.
	if len(w) == 32 && w[0]&0x80 != 0 {
		two256 := new(big.Int).Lsh(big.NewInt(1), 256)
		v.Sub(v, two256)
	}
	return v
}
