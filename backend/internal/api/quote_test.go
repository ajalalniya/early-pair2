package api

import (
	"math/big"
	"strings"
	"testing"

	"github.com/earlypair/backend/internal/chain"
)

const (
	weth  = "0x4200000000000000000000000000000000000006"
	usdc  = "0x833589fcd6edb6e08f4c7c32d4f71b54bda02913"
	token = "0xfffff00000000000000000000000000000000001" // sorts above WETH
)

func baseChain() chain.Chain {
	return chain.Chain{ID: "base", WrappedNative: weth, Stables: []string{usdc}}
}

// TestBuyOnQuoteFirstPoolPaysTheQuote reproduces a pool seen live on Base,
// "WETH / 1KTO100M": WETH sorts below the new token, so it occupies token0.
// A buy must pay WETH and receive the new token — not the reverse, which is
// what deciding by slot order produced.
func TestBuyOnQuoteFirstPoolPaysTheQuote(t *testing.T) {
	t0 := &chain.Token{Address: weth, Symbol: "WETH"}
	t1 := &chain.Token{Address: token, Symbol: "1KTO100M"}
	o := chain.Orient(baseChain(), t0, t1)

	if got := o.Name(); got != "1KTO100M / WETH" {
		t.Errorf("pair name = %q, want the traded token first: %q", got, "1KTO100M / WETH")
	}
	if o.BaseIsToken0 {
		t.Error("WETH in token0 was treated as the traded token")
	}

	r0 := big.NewInt(10) // WETH reserve (token0)
	r1 := big.NewInt(1_000_000)

	in, out, rIn, rOut := tradeSides(o, "buy", r0, r1)
	if in.Symbol != "WETH" || out.Symbol != "1KTO100M" {
		t.Fatalf("buy pays %s and receives %s; a buy must pay WETH and receive 1KTO100M",
			in.Symbol, out.Symbol)
	}
	if rIn.Cmp(r0) != 0 || rOut.Cmp(r1) != 0 {
		t.Errorf("buy used reserveIn=%s reserveOut=%s; the WETH reserve must be the input side",
			rIn, rOut)
	}

	in, out, rIn, rOut = tradeSides(o, "sell", r0, r1)
	if in.Symbol != "1KTO100M" || out.Symbol != "WETH" {
		t.Fatalf("sell pays %s and receives %s; a sell must pay 1KTO100M and receive WETH",
			in.Symbol, out.Symbol)
	}
	if rIn.Cmp(r1) != 0 || rOut.Cmp(r0) != 0 {
		t.Errorf("sell used reserveIn=%s reserveOut=%s", rIn, rOut)
	}
}

func TestBuyOnTokenFirstPoolPaysTheQuote(t *testing.T) {
	t0 := &chain.Token{Address: "0x0000000000000000000000000000000000000001", Symbol: "GROK"}
	t1 := &chain.Token{Address: weth, Symbol: "WETH"}
	o := chain.Orient(baseChain(), t0, t1)

	in, out, _, _ := tradeSides(o, "buy", big.NewInt(1), big.NewInt(1))
	if in.Symbol != "WETH" || out.Symbol != "GROK" {
		t.Errorf("buy pays %s and receives %s; want WETH -> GROK", in.Symbol, out.Symbol)
	}
}

// TestStableBeatsWrappedNative: in a WETH/USDC pool the quote is USDC, so the
// pool reads "WETH / USDC" and buying means buying WETH with USDC.
func TestStableBeatsWrappedNative(t *testing.T) {
	for _, order := range [][2]*chain.Token{
		{{Address: weth, Symbol: "WETH"}, {Address: usdc, Symbol: "USDC"}},
		{{Address: usdc, Symbol: "USDC"}, {Address: weth, Symbol: "WETH"}},
	} {
		o := chain.Orient(baseChain(), order[0], order[1])
		if o.Name() != "WETH / USDC" {
			t.Errorf("slots %s,%s named %q, want %q", order[0].Symbol, order[1].Symbol, o.Name(), "WETH / USDC")
		}
	}
}

func TestUnknownQuoteKeepsSlotOrder(t *testing.T) {
	a := &chain.Token{Address: "0x01", Symbol: "AAA"}
	b := &chain.Token{Address: "0x02", Symbol: "BBB"}
	o := chain.Orient(baseChain(), a, b)
	if !o.BaseIsToken0 || !strings.HasPrefix(o.Name(), "AAA") {
		t.Errorf("with no known quote asset the slot order should stand, got %q", o.Name())
	}
}

func TestAmountOutV2MatchesConstantProduct(t *testing.T) {
	// 1 in against 1000/1000 reserves at 0.30%: 997*1000 / (1000*1000 + 997) = 0.996...
	// Scaled up to avoid integer truncation to zero.
	in := new(big.Int).Mul(big.NewInt(1), big.NewInt(1e18))
	rIn := new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18))
	rOut := new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18))

	got := amountOutV2(in, rIn, rOut, 30)

	// Uniswap v2's getAmountOut with the standard 997/1000 fee.
	inWithFee := new(big.Int).Mul(in, big.NewInt(997))
	num := new(big.Int).Mul(inWithFee, rOut)
	den := new(big.Int).Add(new(big.Int).Mul(rIn, big.NewInt(1000)), inWithFee)
	want := new(big.Int).Div(num, den)

	if got.Cmp(want) != 0 {
		t.Errorf("amountOutV2 = %s, want %s (Uniswap v2 getAmountOut)", got, want)
	}
}

// An approval covers the whole balance so that trading the same token again
// needs no second approval, and the label says so. When the balance cannot be
// read it falls back to the trade's own amount rather than approving blind.
func TestApprovalLabelSaysWhenItCoversMoreThanTheTrade(t *testing.T) {
	usdc := &chain.Token{Address: "0x36", Symbol: "USDC", Decimals: 6}
	trade := big.NewInt(5_000_000)    // 5 USDC
	balance := big.NewInt(42_500_000) // 42.5 USDC

	whole := approvalLabel("Approve %s for Permit2", usdc, balance, trade)
	if !strings.Contains(whole, "42.5 USDC") || !strings.Contains(whole, "once") {
		t.Errorf("label = %q, want the balance and that it is a one-off", whole)
	}
	if just := approvalLabel("Approve %s for Permit2", usdc, trade, trade); strings.Contains(just, "once") {
		t.Errorf("an approval for this trade only should not claim to cover more: %q", just)
	}
}

func TestPermit2AllowanceIsCappedToUint160(t *testing.T) {
	huge := new(big.Int).Lsh(big.NewInt(1), 200)
	if got := capUint160(huge); got.Cmp(maxUint160) != 0 {
		t.Errorf("capUint160(2^200) = %s, want maxUint160", got)
	}
	small := big.NewInt(1_000)
	if got := capUint160(small); got.Cmp(small) != 0 {
		t.Errorf("capUint160 changed a value that already fits: %s", got)
	}
}
