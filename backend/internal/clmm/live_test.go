package clmm

import (
	"context"
	"io"
	"log/slog"
	"math/big"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/config"
	"github.com/earlypair/backend/internal/rpcpool"
)

// Reading a pool's ticks is ABI encoding and bit twiddling against contracts
// this code does not own, so the test that matters runs against a live chain.
// It needs an endpoint and a pool to read, and is skipped without them:
//
//	EARLYPAIR_LIVE_RPC=https://…  EARLYPAIR_LIVE_V4_POOL=0x…(64 hex)
//	EARLYPAIR_LIVE_V3_POOL=0x…(40 hex)  go test ./internal/clmm -run Live -v
func liveEVM(t *testing.T) (*rpcpool.EVM, common.Address) {
	t.Helper()
	url := os.Getenv("EARLYPAIR_LIVE_RPC")
	if url == "" {
		t.Skip("EARLYPAIR_LIVE_RPC is not set")
	}
	ch := chain.Chain{ID: "live", Kind: chain.KindEVM, Enabled: true, BlockTime: 1_000_000_000}
	reg := chain.NewRegistry([]chain.Chain{ch})
	pool, err := rpcpool.New(config.Default().RPC, []config.Endpoint{{
		ID: "live", ChainID: ch.ID, Transport: chain.HTTP, URL: url, RateLimit: 10, Burst: 10,
	}}, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// Multicall3 is at the same address on every chain that has it.
	return pool.EVM(ch.ID), common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11")
}

func checkDistribution(t *testing.T, d *Distribution) {
	t.Helper()
	if d.TickSpacing <= 0 {
		t.Errorf("tick spacing %d", d.TickSpacing)
	}
	if d.Price <= 0 {
		t.Errorf("price %g", d.Price)
	}
	if len(d.Bins) == 0 {
		t.Fatal("no bins: a pool with liquidity must have ranges")
	}

	var active *Bin
	for i := range d.Bins {
		b := &d.Bins[i]
		if b.TickLower >= b.TickUpper {
			t.Errorf("bin %d..%d is empty or inverted", b.TickLower, b.TickUpper)
		}
		if b.Liquidity <= 0 {
			t.Errorf("bin %d..%d has liquidity %g", b.TickLower, b.TickUpper, b.Liquidity)
		}
		if b.PriceLower <= 0 || b.PriceUpper <= b.PriceLower {
			t.Errorf("bin %d..%d prices %g..%g", b.TickLower, b.TickUpper, b.PriceLower, b.PriceUpper)
		}
		if b.Active {
			if active != nil {
				t.Error("two bins claim the current price")
			}
			active = b
		}
	}
	// A pool whose price has run past every range has no active liquidity, and
	// then no bin holds the price. That is a real state, not a failure — it is
	// what a chart should show — so it is only checked when the pool says it
	// has liquidity.
	if d.Liquidity > 0 {
		if active == nil {
			t.Fatal("the pool reports active liquidity but no bin holds the current price")
		}
		if diff := active.Liquidity - d.Liquidity; diff > 1 || diff < -1 {
			t.Errorf("the active bin has L=%g but the pool reports L=%g", active.Liquidity, d.Liquidity)
		}
		if d.Price < active.PriceLower*0.999 || d.Price > active.PriceUpper*1.001 {
			t.Errorf("price %g is outside the active bin %g..%g", d.Price, active.PriceLower, active.PriceUpper)
		}
	} else if active != nil {
		t.Error("a bin claims the current price although the pool has no liquidity there")
	}
	t.Logf("tick %d, price %g, L %g, %d bins, truncated=%v", d.Tick, d.Price, d.Liquidity, len(d.Bins), d.Truncated)
}

func TestLiveV4Distribution(t *testing.T) {
	evm, multicall := liveEVM(t)
	id := os.Getenv("EARLYPAIR_LIVE_V4_POOL")
	view := os.Getenv("EARLYPAIR_LIVE_STATE_VIEW")
	if id == "" || view == "" {
		t.Skip("EARLYPAIR_LIVE_V4_POOL or EARLYPAIR_LIVE_STATE_VIEW is not set")
	}
	spacing := int32(60)
	if v := os.Getenv("EARLYPAIR_LIVE_V4_SPACING"); v != "" {
		n, _ := new(big.Int).SetString(v, 10)
		spacing = int32(n.Int64())
	}
	r := V4PoolWithSpacing{
		V4Pool:      V4Pool{StateView: common.HexToAddress(view), PoolID: common.HexToHash(id)},
		TickSpacing: spacing,
	}
	d, err := Read(context.Background(), evm, multicall, r, 18, 6, true)
	if err != nil {
		t.Fatal(err)
	}
	checkDistribution(t, d)
}

func TestLiveV3Distribution(t *testing.T) {
	evm, multicall := liveEVM(t)
	addr := os.Getenv("EARLYPAIR_LIVE_V3_POOL")
	if addr == "" {
		t.Skip("EARLYPAIR_LIVE_V3_POOL is not set")
	}
	d, err := Read(context.Background(), evm, multicall, V3Pool{Address: common.HexToAddress(addr)}, 18, 6, true)
	if err != nil {
		t.Fatal(err)
	}
	checkDistribution(t, d)
}
