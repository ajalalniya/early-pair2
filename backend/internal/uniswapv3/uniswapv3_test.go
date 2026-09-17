package uniswapv3

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

var (
	usdc = common.HexToAddress("0x3600000000000000000000000000000000000000")
	tok  = common.HexToAddress("0x171a4217b86a807a64eb94757db6849fb4bdbaa0")
)

func swap() Swap {
	return Swap{
		TokenIn: usdc, TokenOut: tok, Fee: big.NewInt(3000),
		Recipient: common.HexToAddress("0x000000000000000000000000000000000000dead"),
		AmountIn:  big.NewInt(1_000_000), MinOut: big.NewInt(900), Deadline: big.NewInt(1_900_000_000),
	}
}

// The deadline rides on multicall, because SwapRouter02's exactInputSingle has
// no deadline field of its own. A swap encoded without it could sit unsigned
// and then be mined at any later price.
func TestEncodeSwapWrapsExactInputSingleInMulticall(t *testing.T) {
	data, err := EncodeSwap(swap())
	if err != nil {
		t.Fatal(err)
	}
	if want := common.FromHex("0x5ae401dc"); !bytes.Equal(data[:4], want) {
		t.Errorf("selector %x, want multicall(uint256,bytes[]) %x", data[:4], want)
	}
	inner := common.FromHex("0x04e45aaf") // exactInputSingle((address,address,uint24,address,uint256,uint256,uint160))
	if !bytes.Contains(data, inner) {
		t.Error("the multicall does not carry an exactInputSingle call")
	}
	if !bytes.Contains(data, common.LeftPadBytes(big.NewInt(1_900_000_000).Bytes(), 32)) {
		t.Error("the deadline is not in the calldata")
	}
}

// The fee tier addresses the pool: without it the router would look up a
// different pool, or none. Zero is never a real v3 tier.
func TestEncodeSwapRejectsUnusableInputs(t *testing.T) {
	cases := map[string]func(*Swap){
		"zero fee":      func(s *Swap) { s.Fee = big.NewInt(0) },
		"nil fee":       func(s *Swap) { s.Fee = nil },
		"zero amount":   func(s *Swap) { s.AmountIn = big.NewInt(0) },
		"no deadline":   func(s *Swap) { s.Deadline = nil },
		"negative min":  func(s *Swap) { s.MinOut = big.NewInt(-1) },
		"absurd amount": func(s *Swap) { s.AmountIn = new(big.Int).Lsh(big.NewInt(1), 200) },
	}
	for name, break_ := range cases {
		s := swap()
		break_(&s)
		if _, err := EncodeSwap(s); err == nil {
			t.Errorf("%s: encoded anyway, want an error", name)
		}
	}
}
