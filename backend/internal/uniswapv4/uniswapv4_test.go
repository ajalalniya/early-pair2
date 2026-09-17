package uniswapv4

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/earlypair/backend/internal/chain"
)

// Keys read from live Robinhood Chain Initialize events, with the PoolIds the
// PoolManager emitted for them.
var (
	longcoln = PoolKey{
		Currency0:   common.Address{},
		Currency1:   common.HexToAddress("0x3Ba6F3a9d3C80debC90f440e2A8eeF14e92B1e18"),
		Fee:         big.NewInt(8388608), // dynamic-fee flag
		TickSpacing: big.NewInt(8),
		Hooks:       common.HexToAddress("0x4e3468951D49f2EEa976eD0D6e75fFCb44a9a544"),
	}
	longcolnID = common.HexToHash("0xb9e1bc755a9ee84650e091a126030c7a3aa9b480754fad5042d15458b1ea328f")

	loongcat = PoolKey{
		Currency0:   common.HexToAddress("0x5fc5360D0400a0Fd4f2af552ADD042D716F1d168"),
		Currency1:   common.HexToAddress("0x6817CA5fFBe0C2aCF0d79176dE787FeDDd5f1e18"),
		Fee:         big.NewInt(8388608),
		TickSpacing: big.NewInt(8),
		Hooks:       common.HexToAddress("0x4e3468951D49f2EEa976eD0D6e75fFCb44a9a544"),
	}
	loongcatID = common.HexToHash("0x1d112550f8ccc9d58db23f29c5033c3c432867bde31d011d2bfffd3ddd48cfe8")
)

func TestPoolKeyHashesToTheOnChainPoolID(t *testing.T) {
	if got := longcoln.ID(); got != longcolnID {
		t.Errorf("native-quoted key: id %s, want %s", got.Hex(), longcolnID.Hex())
	}
	if got := loongcat.ID(); got != loongcatID {
		t.Errorf("token-quoted key: id %s, want %s", got.Hex(), loongcatID.Hex())
	}
}

func word(v *big.Int) []byte {
	b := make([]byte, 32)
	if v.Sign() < 0 {
		v = new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 256), v)
	}
	v.FillBytes(b)
	return b
}

func initializeLog(k PoolKey, id common.Hash) types.Log {
	data := bytes.Join([][]byte{
		word(k.Fee), word(k.TickSpacing), common.LeftPadBytes(k.Hooks.Bytes(), 32),
		word(new(big.Int).Lsh(big.NewInt(1), 96)), word(big.NewInt(0)),
	}, nil)
	return types.Log{
		Topics: []common.Hash{chain.TopicInitializeV4, id,
			common.BytesToHash(k.Currency0.Bytes()), common.BytesToHash(k.Currency1.Bytes())},
		Data: data,
	}
}

func TestKeyFromInitialize(t *testing.T) {
	k, err := KeyFromInitialize(initializeLog(loongcat, loongcatID))
	if err != nil {
		t.Fatal(err)
	}
	if k.ID() != loongcatID || k.TickSpacing.Int64() != 8 {
		t.Errorf("decoded key %+v", k)
	}

	// A negative tick spacing word must decode as negative, not as 2^256-8.
	neg := loongcat
	neg.TickSpacing = big.NewInt(-8)
	if k, err := KeyFromInitialize(initializeLog(neg, neg.ID())); err != nil || k.TickSpacing.Int64() != -8 {
		t.Errorf("negative tick spacing: %v %v", k.TickSpacing, err)
	}

	// A log whose key does not hash to its PoolId is refused.
	if _, err := KeyFromInitialize(initializeLog(loongcat, longcolnID)); err == nil {
		t.Error("accepted an Initialize whose key hashes to a different pool")
	}
}

func TestEncodeExecuteLayouts(t *testing.T) {
	s := Swap{Key: loongcat, ZeroForOne: false, AmountIn: big.NewInt(1_000), MinOut: big.NewInt(900), Deadline: big.NewInt(1_900_000_000)}
	standard, err := EncodeExecute(s)
	if err != nil {
		t.Fatal(err)
	}
	s.ExtraParam = true
	extra, err := EncodeExecute(s)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(standard[:4], common.FromHex("0x3593564c")) {
		t.Errorf("selector %x, want execute(bytes,bytes[],uint256)", standard[:4])
	}
	// The older layout is the standard params plus one uint256 word.
	if len(extra)-len(standard) != 32 {
		t.Errorf("extra-param layout is %d bytes longer, want 32", len(extra)-len(standard))
	}
	// Selling currency1 for currency0: the input is currency1 and nothing is payable.
	if s.In() != loongcat.Currency1 || s.Out() != loongcat.Currency0 || s.PayableValue().Sign() != 0 {
		t.Errorf("in=%s out=%s value=%s", s.In(), s.Out(), s.PayableValue())
	}
}

func TestWrapNativePaysWETHWithValue(t *testing.T) {
	weth := common.HexToAddress("0x0bd7d308f8e1639fab988df18a8011f41eacad73")
	key := loongcat
	key.Currency0 = weth // a WETH pool: WETH in, the token out
	s := Swap{Key: key, ZeroForOne: true, AmountIn: big.NewInt(1_000), MinOut: big.NewInt(9), Deadline: big.NewInt(1), WrapNative: true}
	if s.PayableValue().Int64() != 1_000 {
		t.Errorf("value = %s, want the input paid in native currency", s.PayableValue())
	}
	data, err := EncodeExecute(s)
	if err != nil {
		t.Fatal(err)
	}
	vals, err := routerABI.Methods["execute"].Inputs.Unpack(data[4:])
	if err != nil {
		t.Fatal(err)
	}
	if cmds := vals[0].([]byte); !bytes.Equal(cmds, []byte{cmdWrapETH, cmdV4Swap}) {
		t.Errorf("commands = %x, want WRAP_ETH then V4_SWAP", cmds)
	}
	// The v4 actions settle from the router (SETTLE), not from the user through Permit2.
	v4, err := abi.Arguments{{Type: tBytes}, {Type: tBytesA}}.Unpack(vals[1].([][]byte)[1])
	if err != nil {
		t.Fatal(err)
	}
	if actions := v4[0].([]byte); !bytes.Equal(actions, []byte{actSwapExactInOne, actSettle, actTakeAll}) {
		t.Errorf("actions = %x", actions)
	}

	native := Swap{Key: longcoln, ZeroForOne: true, AmountIn: big.NewInt(1), MinOut: big.NewInt(0), Deadline: big.NewInt(1), WrapNative: true}
	if _, err := EncodeExecute(native); err == nil {
		t.Error("wrapped a swap whose input is already native")
	}
}

func TestNativeInputIsPayable(t *testing.T) {
	s := Swap{Key: longcoln, ZeroForOne: true, AmountIn: big.NewInt(12345), MinOut: big.NewInt(1), Deadline: big.NewInt(1)}
	if s.PayableValue().Int64() != 12345 {
		t.Errorf("value = %s, want the input amount", s.PayableValue())
	}
	if _, err := EncodeExecute(Swap{Key: longcoln, AmountIn: new(big.Int).Lsh(big.NewInt(1), 128), MinOut: big.NewInt(0), Deadline: big.NewInt(1)}); err == nil {
		t.Error("an amount beyond uint128 was encoded")
	}
}
