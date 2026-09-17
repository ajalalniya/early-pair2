// Command v4probe checks the Uniswap v4 swap encoding against a live chain
// without spending anything: it quotes a buy, then simulates the router call
// from a synthetic account whose balance comes from a state override, in both
// parameter layouts. The layout that succeeds is the one the chain's router
// takes. Usage:
//
//	go run ./cmd/v4probe -chain base -pool 0x… -block 123 -wei 1000000000000000
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/config"
	"github.com/earlypair/backend/internal/rpcpool"
	"github.com/earlypair/backend/internal/uniswapv4"
)

func main() {
	configPath := flag.String("config", "config/earlypair.yaml", "config file")
	chainID := flag.String("chain", "", "chain id")
	poolID := flag.String("pool", "", "v4 PoolId")
	block := flag.Uint64("block", 0, "pool creation block")
	weiIn := flag.String("wei", "1000000000000000", "native amount to spend, in wei")
	wrap := flag.Bool("wrap", false, "buy on a WETH pool paying native ETH (WRAP_ETH before the swap)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fail(err)
	}
	reg := cfg.Registry()
	ch, ok := reg.Get(chain.ID(*chainID))
	if !ok || ch.UniswapV4 == nil {
		fail(fmt.Errorf("chain %q has no uniswapV4 config", *chainID))
	}
	pool, err := rpcpool.New(cfg.RPC, cfg.Endpoints, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	evm := pool.EVM(ch.ID)

	var manager common.Address
	for _, f := range ch.Factories {
		if f.Event == "initialize_v4" {
			manager = common.HexToAddress(f.Address)
		}
	}
	key, err := uniswapv4.ResolveKey(ctx, evm, manager, common.HexToHash(*poolID), *block)
	if err != nil {
		fail(err)
	}
	fmt.Printf("key: c0=%s c1=%s fee=%s tickSpacing=%s hooks=%s\n", key.Currency0, key.Currency1, key.Fee, key.TickSpacing, key.Hooks)
	if *wrap {
		wrapBuy(ctx, evm, ch, key, *weiIn)
		return
	}
	if key.Currency0 != (common.Address{}) {
		erc20Layouts(ctx, evm, ch.UniswapV4.UniversalRouter, key)
		return
	}

	amountIn, _ := new(big.Int).SetString(*weiIn, 10)
	out, gas, err := uniswapv4.Quote(ctx, evm, common.HexToAddress(ch.UniswapV4.Quoter), key, true, amountIn)
	if err != nil {
		fail(fmt.Errorf("quote: %w", err))
	}
	fmt.Printf("quote: %s wei in -> %s out, quoter gas %d\n", amountIn, out, gas)

	from := common.HexToAddress("0x00000000000000000000000000000000000e4b1e")
	overrides := map[string]any{from.Hex(): map[string]any{"balance": (*hexutil.Big)(new(big.Int).Mul(amountIn, big.NewInt(100)))}}
	minOut := new(big.Int).Div(new(big.Int).Mul(out, big.NewInt(99)), big.NewInt(100))
	router := common.HexToAddress(ch.UniswapV4.UniversalRouter)

	for _, extra := range []bool{false, true} {
		swap := uniswapv4.Swap{Key: key, ZeroForOne: true, AmountIn: amountIn, MinOut: minOut,
			Deadline: big.NewInt(time.Now().Add(5 * time.Minute).Unix()), ExtraParam: extra}
		data, err := uniswapv4.EncodeExecute(swap)
		if err != nil {
			fail(err)
		}
		_, simErr := evm.Simulate(ctx, from, router, data, swap.PayableValue(), overrides)
		layout := "standard"
		if extra {
			layout = "extra-uint256"
		}
		if simErr != nil {
			fmt.Printf("simulate %-15s REVERT: %s %s\n", layout, rpcpool.DecodeRevert(simErr), simErr)
		} else {
			fmt.Printf("simulate %-15s OK\n", layout)
		}
		// A too-high minimum must revert: proof the guarantee is enforced on-chain.
		swap.MinOut = new(big.Int).Mul(out, big.NewInt(2))
		data, _ = uniswapv4.EncodeExecute(swap)
		if _, err := evm.Simulate(ctx, from, router, data, swap.PayableValue(), overrides); err != nil {
			fmt.Printf("simulate %-15s with 2x minimum: reverts as it must\n", layout)
		} else {
			fmt.Printf("simulate %-15s with 2x minimum: DID NOT REVERT\n", layout)
		}
	}
}

// wrapBuy simulates buying the token of a wrapped-native pool with native
// currency: WRAP_ETH, then the swap settled from the router's own WETH.
func wrapBuy(ctx context.Context, evm *rpcpool.EVM, ch chain.Chain, key uniswapv4.PoolKey, weiIn string) {
	weth := common.HexToAddress(ch.WrappedNative)
	zeroForOne := key.Currency0 == weth
	if !zeroForOne && key.Currency1 != weth {
		fail(fmt.Errorf("pool has no %s side", weth))
	}
	amountIn, _ := new(big.Int).SetString(weiIn, 10)
	out, _, err := uniswapv4.Quote(ctx, evm, common.HexToAddress(ch.UniswapV4.Quoter), key, zeroForOne, amountIn)
	if err != nil {
		fail(fmt.Errorf("quote: %w", err))
	}
	fmt.Printf("quote: %s wei of WETH in -> %s out\n", amountIn, out)
	from := common.HexToAddress("0x00000000000000000000000000000000000e4b1e")
	overrides := map[string]any{from.Hex(): map[string]any{"balance": (*hexutil.Big)(new(big.Int).Mul(amountIn, big.NewInt(100)))}}
	router := common.HexToAddress(ch.UniswapV4.UniversalRouter)
	for _, minOut := range []*big.Int{new(big.Int).Div(new(big.Int).Mul(out, big.NewInt(99)), big.NewInt(100)), new(big.Int).Mul(out, big.NewInt(2))} {
		swap := uniswapv4.Swap{Key: key, ZeroForOne: zeroForOne, AmountIn: amountIn, MinOut: minOut,
			Deadline: big.NewInt(time.Now().Add(5 * time.Minute).Unix()), ExtraParam: ch.UniswapV4.ExtraSwapParam, WrapNative: true}
		data, err := uniswapv4.EncodeExecute(swap)
		if err != nil {
			fail(err)
		}
		_, simErr := evm.Simulate(ctx, from, router, data, swap.PayableValue(), overrides)
		fmt.Printf("wrap-and-swap, min %s: err=%v\n", minOut, simErr)
	}
}

// erc20Layouts tells the router's parameter layout apart on a pool whose
// currency0 is a token. From an account with no allowance, the layout the router
// takes decodes cleanly and fails where it pulls the input through Permit2; the
// other misreads hookData's offset and fails decoding, with different revert data.
func erc20Layouts(ctx context.Context, evm *rpcpool.EVM, routerAddr string, key uniswapv4.PoolKey) {
	from := common.HexToAddress("0x00000000000000000000000000000000000e4b1e")
	router := common.HexToAddress(routerAddr)
	for _, extra := range []bool{false, true} {
		swap := uniswapv4.Swap{Key: key, ZeroForOne: true, AmountIn: big.NewInt(1_000_000), MinOut: big.NewInt(0),
			Deadline: big.NewInt(time.Now().Add(5 * time.Minute).Unix()), ExtraParam: extra}
		data, err := uniswapv4.EncodeExecute(swap)
		if err != nil {
			fail(err)
		}
		_, simErr := evm.Simulate(ctx, from, router, data, nil, nil)
		var de rpc.DataError
		revertData := "none"
		if errors.As(simErr, &de) {
			revertData = fmt.Sprint(de.ErrorData())
		}
		fmt.Printf("erc20 %-15v err=%v data=%s\n", map[bool]string{false: "standard", true: "extra-uint256"}[extra], simErr, revertData)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "v4probe:", err)
	os.Exit(1)
}
