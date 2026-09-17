// Package uniswapv4 quotes and encodes swaps against Uniswap v4 pools.
//
// A v4 pool lives inside its chain's PoolManager and is addressed by a PoolKey
// — both currencies, the fee, the tick spacing and the hook contract — whose
// hash is the PoolId the rest of the service stores. Swaps go through Uniswap's
// Universal Router, and quotes come from Uniswap's V4Quoter, which simulates
// the swap in the PoolManager itself: hooks, dynamic fees and every tick crossed
// included. Nothing here approximates a price from pool state.
package uniswapv4

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/rpcpool"
)

// PoolKey identifies a v4 pool. Its hash is the PoolId.
type PoolKey struct {
	Currency0   common.Address
	Currency1   common.Address
	Fee         *big.Int // uint24, raw: hundredths of a bip, or the dynamic-fee flag
	TickSpacing *big.Int // int24
	Hooks       common.Address
}

var (
	tAddress = mustType("address", nil)
	tUint24  = mustType("uint24", nil)
	tInt24   = mustType("int24", nil)
	tUint256 = mustType("uint256", nil)
	tBytes   = mustType("bytes", nil)
	tBytesA  = mustType("bytes[]", nil)
	tBool    = mustType("bool", nil)
)

func mustType(t string, components []abi.ArgumentMarshaling) abi.Type {
	ty, err := abi.NewType(t, "", components)
	if err != nil {
		panic("uniswapv4: bad ABI type " + t + ": " + err.Error())
	}
	return ty
}

func mustABI(s string) abi.ABI {
	a, err := abi.JSON(strings.NewReader(s))
	if err != nil {
		panic("uniswapv4: bad built-in ABI: " + err.Error())
	}
	return a
}

// ID is keccak256(abi.encode(key)), the PoolId.
func (k PoolKey) ID() common.Hash {
	enc, err := abi.Arguments{{Type: tAddress}, {Type: tAddress}, {Type: tUint24}, {Type: tInt24}, {Type: tAddress}}.
		Pack(k.Currency0, k.Currency1, k.Fee, k.TickSpacing, k.Hooks)
	if err != nil {
		return common.Hash{}
	}
	return crypto.Keccak256Hash(enc)
}

// KeyFromInitialize reads a pool's key from its Initialize event and checks
// that the key hashes to the event's PoolId, so a malformed or foreign log can
// never yield a key for a different pool.
func KeyFromInitialize(l types.Log) (PoolKey, error) {
	if len(l.Topics) < 4 || l.Topics[0] != chain.TopicInitializeV4 || len(l.Data) < 96 {
		return PoolKey{}, errors.New("uniswapv4: not an Initialize log")
	}
	k := PoolKey{
		Currency0:   common.BytesToAddress(l.Topics[2].Bytes()),
		Currency1:   common.BytesToAddress(l.Topics[3].Bytes()),
		Fee:         new(big.Int).SetBytes(l.Data[0:32]),
		TickSpacing: signedWord(l.Data[32:64]),
		Hooks:       common.BytesToAddress(l.Data[64:96]),
	}
	if k.ID() != l.Topics[1] {
		return PoolKey{}, fmt.Errorf("uniswapv4: Initialize key does not hash to pool %s", l.Topics[1].Hex())
	}
	return k, nil
}

func signedWord(b []byte) *big.Int {
	v := new(big.Int).SetBytes(b)
	if len(b) == 32 && b[0]&0x80 != 0 {
		v.Sub(v, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	return v
}

// ResolveKey fetches a pool's Initialize event from its creation block.
func ResolveKey(ctx context.Context, evm *rpcpool.EVM, manager common.Address, poolID common.Hash, block uint64) (PoolKey, error) {
	logs, err := evm.FilterLogsHedged(ctx, rpcpool.FilterQuery{
		FromBlock: block, ToBlock: block,
		Addresses: []common.Address{manager},
		Topics:    [][]common.Hash{{chain.TopicInitializeV4}, {poolID}},
	})
	if err != nil {
		return PoolKey{}, fmt.Errorf("uniswapv4: read Initialize for %s: %w", poolID.Hex(), err)
	}
	for _, l := range logs {
		if k, err := KeyFromInitialize(l); err == nil {
			return k, nil
		}
	}
	return PoolKey{}, fmt.Errorf("uniswapv4: no Initialize for %s in block %d", poolID.Hex(), block)
}

// ------------------------------------------------------------------ quoting

var quoterABI = mustABI(`[{"name":"quoteExactInputSingle","type":"function","stateMutability":"nonpayable",
 "inputs":[{"name":"params","type":"tuple","components":[
   {"name":"poolKey","type":"tuple","components":[
     {"name":"currency0","type":"address"},{"name":"currency1","type":"address"},
     {"name":"fee","type":"uint24"},{"name":"tickSpacing","type":"int24"},{"name":"hooks","type":"address"}]},
   {"name":"zeroForOne","type":"bool"},{"name":"exactAmount","type":"uint128"},{"name":"hookData","type":"bytes"}]}],
 "outputs":[{"name":"amountOut","type":"uint256"},{"name":"gasEstimate","type":"uint256"}]}]`)

type quoteParams struct {
	PoolKey     PoolKey
	ZeroForOne  bool
	ExactAmount *big.Int
	HookData    []byte
}

// maxUint128 bounds amounts: the router and quoter take uint128.
var maxUint128 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))

// Quote asks the V4Quoter for the output of an exact-input single-pool swap.
// The quoter runs the real swap in the PoolManager and reverts with the
// result, so the answer includes the hook, the fee in force and price impact.
func Quote(ctx context.Context, evm *rpcpool.EVM, quoter common.Address, key PoolKey, zeroForOne bool, amountIn *big.Int) (amountOut *big.Int, gasEstimate uint64, err error) {
	if amountIn.Sign() <= 0 || amountIn.Cmp(maxUint128) > 0 {
		return nil, 0, errors.New("uniswapv4: amount out of range")
	}
	data, err := quoterABI.Pack("quoteExactInputSingle", quoteParams{PoolKey: key, ZeroForOne: zeroForOne, ExactAmount: amountIn, HookData: []byte{}})
	if err != nil {
		return nil, 0, fmt.Errorf("uniswapv4: encode quote: %w", err)
	}
	out, err := evm.CallContract(ctx, quoter, data, nil, nil)
	if err != nil {
		return nil, 0, err
	}
	vals, err := quoterABI.Unpack("quoteExactInputSingle", out)
	if err != nil || len(vals) != 2 {
		return nil, 0, fmt.Errorf("uniswapv4: decode quote: %v", err)
	}
	amountOut, _ = vals[0].(*big.Int)
	gas, _ := vals[1].(*big.Int)
	if amountOut == nil {
		return nil, 0, errors.New("uniswapv4: quote returned no amount")
	}
	if gas != nil && gas.IsUint64() {
		gasEstimate = gas.Uint64()
	}
	return amountOut, gasEstimate, nil
}

// ------------------------------------------------------------------ swap encoding

// Universal Router command and v4 router action codes.
const (
	cmdWrapETH        = 0x0b
	cmdV4Swap         = 0x10
	actSwapExactInOne = 0x06
	actSettle         = 0x0b
	actSettleAll      = 0x0c
	actTakeAll        = 0x0f
)

// addressThis is the routers' placeholder for "the router itself".
var addressThis = common.BigToAddress(big.NewInt(2))

var routerABI = mustABI(`[{"name":"execute","type":"function","stateMutability":"payable",
 "inputs":[{"name":"commands","type":"bytes"},{"name":"inputs","type":"bytes[]"},{"name":"deadline","type":"uint256"}],"outputs":[]}]`)

var keyComponents = []abi.ArgumentMarshaling{
	{Name: "currency0", Type: "address"}, {Name: "currency1", Type: "address"},
	{Name: "fee", Type: "uint24"}, {Name: "tickSpacing", Type: "int24"}, {Name: "hooks", Type: "address"},
}

var (
	// ExactInputSingleParams as Uniswap's v4-periphery defines it.
	exactInSingle = mustType("tuple", []abi.ArgumentMarshaling{
		{Name: "poolKey", Type: "tuple", Components: keyComponents},
		{Name: "zeroForOne", Type: "bool"},
		{Name: "amountIn", Type: "uint128"},
		{Name: "amountOutMinimum", Type: "uint128"},
		{Name: "hookData", Type: "bytes"},
	})
	// The same with one more uint256 before hookData, as the routers deployed on
	// Robinhood Chain (which calls it minHopPriceX36) and Arc expect. Sending
	// the standard layout to those shifts every later word: hookData is read
	// from the wrong offset, so a hookless pool swaps anyway and a hooked one
	// reverts. Zero is the no-op value on both.
	exactInSingleExtra = mustType("tuple", []abi.ArgumentMarshaling{
		{Name: "poolKey", Type: "tuple", Components: keyComponents},
		{Name: "zeroForOne", Type: "bool"},
		{Name: "amountIn", Type: "uint128"},
		{Name: "amountOutMinimum", Type: "uint128"},
		{Name: "extraParam", Type: "uint256"},
		{Name: "hookData", Type: "bytes"},
	})
)

type exactInSingleParams struct {
	PoolKey          PoolKey
	ZeroForOne       bool
	AmountIn         *big.Int
	AmountOutMinimum *big.Int
	HookData         []byte
}

type exactInSingleExtraParams struct {
	PoolKey          PoolKey
	ZeroForOne       bool
	AmountIn         *big.Int
	AmountOutMinimum *big.Int
	ExtraParam       *big.Int
	HookData         []byte
}

// Swap describes an exact-input single-pool swap.
type Swap struct {
	Key        PoolKey
	ZeroForOne bool
	AmountIn   *big.Int
	MinOut     *big.Int // enforced by the router: the swap reverts below it
	Deadline   *big.Int // unix seconds
	// ExtraParam: the router takes the older exact-input layout, with one more
	// uint256 before hookData. See exactInSingleExtra.
	ExtraParam bool
	// WrapNative pays a wrapped-native input (WETH) with native currency: the
	// router wraps the transaction's value, then settles the swap from its own
	// balance. No approval is involved.
	WrapNative bool
}

// In and Out are the currencies paid and received.
func (s Swap) In() common.Address {
	if s.ZeroForOne {
		return s.Key.Currency0
	}
	return s.Key.Currency1
}

func (s Swap) Out() common.Address {
	if s.ZeroForOne {
		return s.Key.Currency1
	}
	return s.Key.Currency0
}

// PayableValue is the native amount the transaction must carry: the input
// when paying in native currency, which the router settles from msg.value.
func (s Swap) PayableValue() *big.Int {
	if s.In() == (common.Address{}) || s.WrapNative {
		return new(big.Int).Set(s.AmountIn)
	}
	return big.NewInt(0)
}

// EncodeExecute builds Universal Router execute() calldata for the swap: one
// V4_SWAP command running SWAP_EXACT_IN_SINGLE, then SETTLE_ALL for the input
// and TAKE_ALL for the output. TAKE_ALL carries the minimum received, so the
// guarantee is enforced twice: by the swap and by the take.
//
// With WrapNative, a WRAP_ETH command runs first, wrapping the value into WETH
// held by the router, and the input is settled with SETTLE paid by the router
// instead of SETTLE_ALL pulled from the user through Permit2.
func EncodeExecute(s Swap) ([]byte, error) {
	if s.WrapNative && s.In() == (common.Address{}) {
		return nil, errors.New("uniswapv4: the input is already the native currency")
	}
	if s.AmountIn == nil || s.AmountIn.Sign() <= 0 || s.AmountIn.Cmp(maxUint128) > 0 {
		return nil, errors.New("uniswapv4: amountIn out of range")
	}
	if s.MinOut == nil || s.MinOut.Sign() < 0 || s.MinOut.Cmp(maxUint128) > 0 {
		return nil, errors.New("uniswapv4: minOut out of range")
	}

	var swapParam []byte
	var err error
	if s.ExtraParam {
		swapParam, err = abi.Arguments{{Type: exactInSingleExtra}}.Pack(exactInSingleExtraParams{
			PoolKey: s.Key, ZeroForOne: s.ZeroForOne, AmountIn: s.AmountIn, AmountOutMinimum: s.MinOut,
			ExtraParam: big.NewInt(0), HookData: []byte{},
		})
	} else {
		swapParam, err = abi.Arguments{{Type: exactInSingle}}.Pack(exactInSingleParams{
			PoolKey: s.Key, ZeroForOne: s.ZeroForOne, AmountIn: s.AmountIn, AmountOutMinimum: s.MinOut, HookData: []byte{},
		})
	}
	if err != nil {
		return nil, fmt.Errorf("uniswapv4: encode swap params: %w", err)
	}
	settleAction := byte(actSettleAll)
	var settle []byte
	if s.WrapNative {
		settleAction = actSettle
		settle, err = abi.Arguments{{Type: tAddress}, {Type: tUint256}, {Type: tBool}}.Pack(s.In(), s.AmountIn, false)
	} else {
		settle, err = abi.Arguments{{Type: tAddress}, {Type: tUint256}}.Pack(s.In(), s.AmountIn)
	}
	if err != nil {
		return nil, err
	}
	take, err := abi.Arguments{{Type: tAddress}, {Type: tUint256}}.Pack(s.Out(), s.MinOut)
	if err != nil {
		return nil, err
	}
	v4Input, err := abi.Arguments{{Type: tBytes}, {Type: tBytesA}}.Pack(
		[]byte{actSwapExactInOne, settleAction, actTakeAll},
		[][]byte{swapParam, settle, take})
	if err != nil {
		return nil, fmt.Errorf("uniswapv4: encode v4 input: %w", err)
	}
	if !s.WrapNative {
		return routerABI.Pack("execute", []byte{cmdV4Swap}, [][]byte{v4Input}, s.Deadline)
	}
	wrap, err := abi.Arguments{{Type: tAddress}, {Type: tUint256}}.Pack(addressThis, s.AmountIn)
	if err != nil {
		return nil, err
	}
	return routerABI.Pack("execute", []byte{cmdWrapETH, cmdV4Swap}, [][]byte{wrap, v4Input}, s.Deadline)
}

// ------------------------------------------------------------------ Permit2

// The Universal Router pulls ERC-20 input through Permit2: the token approves
// Permit2, and Permit2 approves the router. Both are plain transactions here —
// no off-chain signature — so every step is one the wallet shows and signs.
var permit2ABI = mustABI(`[
 {"name":"allowance","type":"function","stateMutability":"view",
  "inputs":[{"name":"user","type":"address"},{"name":"token","type":"address"},{"name":"spender","type":"address"}],
  "outputs":[{"name":"amount","type":"uint160"},{"name":"expiration","type":"uint48"},{"name":"nonce","type":"uint48"}]},
 {"name":"approve","type":"function","stateMutability":"nonpayable",
  "inputs":[{"name":"token","type":"address"},{"name":"spender","type":"address"},{"name":"amount","type":"uint160"},{"name":"expiration","type":"uint48"}],
  "outputs":[]}]`)

// Permit2Allowance reads how much, and until when, spender may pull token for user.
func Permit2Allowance(ctx context.Context, evm *rpcpool.EVM, permit2, user, token, spender common.Address) (amount *big.Int, expiration uint64, err error) {
	data, err := permit2ABI.Pack("allowance", user, token, spender)
	if err != nil {
		return nil, 0, err
	}
	out, err := evm.CallContract(ctx, permit2, data, nil, nil)
	if err != nil {
		return nil, 0, err
	}
	vals, err := permit2ABI.Unpack("allowance", out)
	if err != nil || len(vals) != 3 {
		return nil, 0, fmt.Errorf("uniswapv4: decode permit2 allowance: %v", err)
	}
	amount, _ = vals[0].(*big.Int)
	if exp, ok := vals[1].(*big.Int); ok && exp.IsUint64() {
		expiration = exp.Uint64()
	}
	if amount == nil {
		amount = big.NewInt(0)
	}
	return amount, expiration, nil
}

// EncodePermit2Approve lets spender pull up to amount of token until expiration.
func EncodePermit2Approve(token, spender common.Address, amount *big.Int, expiration uint64) ([]byte, error) {
	return permit2ABI.Pack("approve", token, spender, amount, new(big.Int).SetUint64(expiration))
}
