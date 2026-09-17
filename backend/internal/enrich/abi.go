package enrich

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// Minimal ABIs. Only the handful of read methods enrichment actually calls are
// declared, so there is no code generation step and no dependency on a DEX's
// published artifacts.
const (
	erc20ABI = `[
      {"name":"name","outputs":[{"type":"string"}],"inputs":[],"stateMutability":"view","type":"function"},
      {"name":"symbol","outputs":[{"type":"string"}],"inputs":[],"stateMutability":"view","type":"function"},
      {"name":"decimals","outputs":[{"type":"uint8"}],"inputs":[],"stateMutability":"view","type":"function"},
      {"name":"totalSupply","outputs":[{"type":"uint256"}],"inputs":[],"stateMutability":"view","type":"function"},
      {"name":"balanceOf","outputs":[{"type":"uint256"}],"inputs":[{"type":"address"}],"stateMutability":"view","type":"function"},
      {"name":"owner","outputs":[{"type":"address"}],"inputs":[],"stateMutability":"view","type":"function"}
    ]`

	// Some older tokens return a bytes32 symbol instead of a string, which a
	// string decoder simply fails on. Decoding both is the difference between a
	// named pair and a pair of question marks.
	erc20Bytes32ABI = `[
      {"name":"symbol","outputs":[{"type":"bytes32"}],"inputs":[],"stateMutability":"view","type":"function"},
      {"name":"name","outputs":[{"type":"bytes32"}],"inputs":[],"stateMutability":"view","type":"function"}
    ]`

	pairV2ABI = `[
      {"name":"getReserves","outputs":[{"type":"uint112"},{"type":"uint112"},{"type":"uint32"}],"inputs":[],"stateMutability":"view","type":"function"},
      {"name":"totalSupply","outputs":[{"type":"uint256"}],"inputs":[],"stateMutability":"view","type":"function"},
      {"name":"token0","outputs":[{"type":"address"}],"inputs":[],"stateMutability":"view","type":"function"},
      {"name":"token1","outputs":[{"type":"address"}],"inputs":[],"stateMutability":"view","type":"function"}
    ]`

	poolV3ABI = `[
      {"name":"slot0","outputs":[{"type":"uint160"},{"type":"int24"},{"type":"uint16"},{"type":"uint16"},{"type":"uint16"},{"type":"uint8"},{"type":"bool"}],"inputs":[],"stateMutability":"view","type":"function"},
      {"name":"liquidity","outputs":[{"type":"uint128"}],"inputs":[],"stateMutability":"view","type":"function"}
    ]`

	multicall3ABI = `[
      {"name":"aggregate3","inputs":[{"components":[{"name":"target","type":"address"},{"name":"allowFailure","type":"bool"},{"name":"callData","type":"bytes"}],"name":"calls","type":"tuple[]"}],"outputs":[{"components":[{"name":"success","type":"bool"},{"name":"returnData","type":"bytes"}],"name":"returnData","type":"tuple[]"}],"stateMutability":"payable","type":"function"}
    ]`
)

var (
	erc20        = mustABI(erc20ABI)
	erc20Bytes32 = mustABI(erc20Bytes32ABI)
	pairV2       = mustABI(pairV2ABI)
	poolV3       = mustABI(poolV3ABI)
	multicall3   = mustABI(multicall3ABI)
)

func mustABI(s string) abi.ABI {
	a, err := abi.JSON(strings.NewReader(s))
	if err != nil {
		panic("enrich: bad built-in ABI: " + err.Error())
	}
	return a
}

// call3 mirrors Multicall3's Call3 struct.
type call3 struct {
	Target       common.Address `json:"target"`
	AllowFailure bool           `json:"allowFailure"`
	CallData     []byte         `json:"callData"`
}

type result3 struct {
	Success    bool   `json:"success"`
	ReturnData []byte `json:"returnData"`
}

func packMulticall(calls []call3) ([]byte, error) {
	return multicall3.Pack("aggregate3", calls)
}

func unpackMulticall(data []byte) ([]result3, error) {
	vals, err := multicall3.Unpack("aggregate3", data)
	if err != nil {
		return nil, err
	}
	if len(vals) == 0 {
		return nil, fmt.Errorf("enrich: empty multicall response")
	}
	raw, ok := vals[0].([]struct {
		Success    bool   `json:"success"`
		ReturnData []byte `json:"returnData"`
	})
	if !ok {
		return nil, fmt.Errorf("enrich: unexpected multicall response shape %T", vals[0])
	}
	out := make([]result3, len(raw))
	for i, r := range raw {
		out[i] = result3{Success: r.Success, ReturnData: r.ReturnData}
	}
	return out, nil
}

// decodeString reads a token's string return, falling back to the bytes32 form
// used by several pre-standard tokens.
func decodeString(method string, data []byte) (string, bool) {
	if len(data) == 0 {
		return "", false
	}
	if vals, err := erc20.Unpack(method, data); err == nil && len(vals) > 0 {
		if s, ok := vals[0].(string); ok {
			return strings.TrimSpace(s), s != ""
		}
	}
	if vals, err := erc20Bytes32.Unpack(method, data); err == nil && len(vals) > 0 {
		if b, ok := vals[0].([32]byte); ok {
			s := strings.TrimRight(string(b[:]), "\x00")
			s = strings.TrimSpace(s)
			return s, s != ""
		}
	}
	return "", false
}

func decodeUint8(method string, data []byte) (int, bool) {
	vals, err := erc20.Unpack(method, data)
	if err != nil || len(vals) == 0 {
		return 0, false
	}
	switch v := vals[0].(type) {
	case uint8:
		return int(v), true
	case *big.Int:
		return int(v.Int64()), true
	}
	return 0, false
}

func decodeBigInt(a abi.ABI, method string, data []byte) (*big.Int, bool) {
	vals, err := a.Unpack(method, data)
	if err != nil || len(vals) == 0 {
		return nil, false
	}
	n, ok := vals[0].(*big.Int)
	return n, ok
}

// decodeReserves reads a v2 getReserves() response.
func decodeReserves(data []byte) (*big.Int, *big.Int, bool) {
	vals, err := pairV2.Unpack("getReserves", data)
	if err != nil || len(vals) < 2 {
		return nil, nil, false
	}
	r0, ok0 := vals[0].(*big.Int)
	r1, ok1 := vals[1].(*big.Int)
	return r0, r1, ok0 && ok1
}
