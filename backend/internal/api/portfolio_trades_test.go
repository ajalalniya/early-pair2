package api

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/rpcpool"
)

var (
	tWallet  = common.HexToAddress("0x00000000000000000000000000000000000e4b1e")
	tRouter  = common.HexToAddress("0x8876789976decbfcbbbe364623c63652db8c0904")
	tManager = common.HexToAddress("0x8366a39cc670b4001a1121b8f6a443a643e40951")
	tUSDG    = "0x5fc5360d0400a0fd4f2af552add042d716f1d168"
	tToken   = "0x3ba6f3a9d3c80debc90f440e2a8eef14e92b1e18"
	tPoolID  = "0xb9e1bc755a9ee84650e091a126030c7a3aa9b480754fad5042d15458b1ea328f"
)

func tradeChain() chain.Chain {
	return chain.Chain{
		ID: "rbn", Stables: []string{tUSDG}, WrappedNative: "0x0bd7d308f8e1639fab988df18a8011f41eacad73",
		RouterV2:  "0x0000000000000000000000000000000000000abc",
		UniswapV4: &chain.UniswapV4{UniversalRouter: tRouter.Hex()},
		Factories: []chain.Factory{{Address: tManager.Hex(), Event: chain.EventInitializeV4}},
	}
}

func signedWord(v int64) []byte {
	b := make([]byte, 32)
	n := big.NewInt(v)
	if n.Sign() < 0 {
		n.Add(new(big.Int).Lsh(big.NewInt(1), 256), n)
	}
	n.FillBytes(b)
	return b
}

// v4SwapLog is a PoolManager Swap with swapper-side amounts, as the chain emits.
func v4SwapLog(pool string, amount0, amount1 int64) types.Log {
	data := append(append(signedWord(amount0), signedWord(amount1)...), make([]byte, 32*4)...)
	return types.Log{
		Address: tManager,
		Topics:  []common.Hash{chain.TopicSwapV4, common.HexToHash(pool), common.BytesToHash(tRouter.Bytes())},
		Data:    data,
	}
}

func TestVerifyTradeV4NativeBuy(t *testing.T) {
	ch := tradeChain()
	p := chain.Pool{
		ChainID: "rbn", Address: tPoolID,
		Token0: &chain.Token{Address: chain.NativeAddress, Symbol: "ETH", Decimals: 18},
		Token1: &chain.Token{Address: tToken, Symbol: "LONGCOLN", Decimals: 18},
	}
	// The swapper paid 1e15 wei of ETH (currency0) and received 5e20 LONGCOLN.
	tx := rpcpool.MinedTx{From: tWallet, To: &tRouter, Success: true, BlockNumber: 42,
		Logs: []types.Log{v4SwapLog(tPoolID, -1_000_000_000_000_000, 500_000_000_000_000_000)}}
	tx.Logs[0].Data = append(append(signedWord(-1_000_000_000_000_000), bigWord("500000000000000000000")...), make([]byte, 32*4)...)

	trade, err := verifyTrade(ch, p, common.HexToHash("0x01"), tx)
	if err != nil {
		t.Fatal(err)
	}
	if trade.Side != "buy" || !strings.EqualFold(trade.Token, tToken) || trade.TokenAmount != "500000000000000000000" {
		t.Errorf("trade = %+v", trade)
	}
	if trade.QuoteAmount != "1000000000000000" || !chain.IsNative(trade.QuoteToken) || !strings.EqualFold(trade.Wallet, tWallet.Hex()) {
		t.Errorf("quote side = %s %s, wallet %s", trade.QuoteAmount, trade.QuoteToken, trade.Wallet)
	}
}

func bigWord(dec string) []byte {
	v, _ := new(big.Int).SetString(dec, 10)
	b := make([]byte, 32)
	v.FillBytes(b)
	return b
}

func TestVerifyTradeV2SellRecordsTheTaxedTransfer(t *testing.T) {
	ch := tradeChain()
	router := common.HexToAddress(ch.RouterV2)
	pair := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	// token0 = the traded token (lower address), token1 = USDG.
	token := "0x0000000000000000000000000000000000000011"
	p := chain.Pool{ChainID: "rbn", Address: pair.Hex(),
		Token0: &chain.Token{Address: token, Symbol: "TAX", Decimals: 18},
		Token1: &chain.Token{Address: tUSDG, Symbol: "USDG", Decimals: 6}}

	// The wallet sent 1,000 TAX; the pool saw 950 after a 5% transfer tax and
	// paid out 20 USDG.
	swap := types.Log{Address: pair, Topics: []common.Hash{chain.TopicSwapV2, {}, common.BytesToHash(tWallet.Bytes())},
		Data: append(append(append(bigWord("950"), bigWord("0")...), bigWord("0")...), bigWord("20000000")...)}
	transfer := types.Log{Address: common.HexToAddress(token),
		Topics: []common.Hash{chain.TopicTransfer, common.BytesToHash(tWallet.Bytes()), common.BytesToHash(pair.Bytes())},
		Data:   bigWord("1000")}

	trade, err := verifyTrade(ch, p, common.HexToHash("0x02"), rpcpool.MinedTx{From: tWallet, To: &router, Success: true, Logs: []types.Log{transfer, swap}})
	if err != nil {
		t.Fatal(err)
	}
	if trade.Side != "sell" || trade.TokenAmount != "1000" || trade.QuoteAmount != "20000000" {
		t.Errorf("trade = %+v, want a sell of the 1000 sent for 20 USDG", trade)
	}
}

func TestVerifyTradeRefusesWhatItCannotVerify(t *testing.T) {
	ch := tradeChain()
	p := chain.Pool{ChainID: "rbn", Address: tPoolID,
		Token0: &chain.Token{Address: chain.NativeAddress}, Token1: &chain.Token{Address: tToken}}
	good := []types.Log{v4SwapLog(tPoolID, -1000, 5000)}
	other := common.HexToAddress("0x0000000000000000000000000000000000000bad")

	cases := map[string]rpcpool.MinedTx{
		"reverted":       {From: tWallet, To: &tRouter, Success: false, Logs: good},
		"unknown router": {From: tWallet, To: &other, Success: true, Logs: good},
		"other pool":     {From: tWallet, To: &tRouter, Success: true, Logs: []types.Log{v4SwapLog("0x"+strings.Repeat("ab", 32), -1000, 5000)}},
	}
	for name, tx := range cases {
		if _, err := verifyTrade(ch, p, common.HexToHash("0x03"), tx); err == nil {
			t.Errorf("%s: recorded a trade it could not verify", name)
		}
	}
}
