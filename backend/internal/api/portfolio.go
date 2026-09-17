package api

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/earlypair/backend/internal/chain"
)

const balanceABI = `[
  {"name":"balanceOf","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"}],"stateMutability":"view","type":"function"},
  {"name":"decimals","inputs":[],"outputs":[{"type":"uint8"}],"stateMutability":"view","type":"function"}
]`

var balanceOf = mustABI(balanceABI)

// FundsAsset is one balance line in the "Available funds" card.
type FundsAsset struct {
	Symbol   string   `json:"symbol"`
	Address  string   `json:"address,omitempty"`
	Amount   string   `json:"amount"`
	Decimals int      `json:"decimals"`
	USD      *float64 `json:"usd"`
	IsStable bool     `json:"isStable"`
	IsNative bool     `json:"isNative"`
	// Unavailable: the balance could not be read. The line is still listed —
	// dropping it read as "you hold none" — with no amount or dollar value.
	Unavailable bool `json:"unavailable,omitempty"`
}

// FundsChain groups a chain's balances.
type FundsChain struct {
	ChainID  chain.ID     `json:"chainId"`
	Name     string       `json:"name"`
	Tag      string       `json:"tag"`
	Assets   []FundsAsset `json:"assets"`
	TotalUSD *float64     `json:"totalUsd"`
}

// handlePortfolioFunds reads native and stable balances straight from the
// chains. It only reports a USD figure for assets whose price is actually
// established — a stable at one dollar, or the native token when the reference
// pool has given us a price. Everything else carries a null, which the UI shows
// as an em dash.
func (s *Server) handlePortfolioFunds(w http.ResponseWriter, r *http.Request) {
	address := AddressFrom(r.Context())
	if !common.IsHexAddress(address) {
		writeError(w, http.StatusBadRequest, "bad_address", "session address is not valid")
		return
	}
	owner := common.HexToAddress(address)

	chains := s.reg.All()
	results := make([]FundsChain, len(chains))

	var wg sync.WaitGroup
	for i, ch := range chains {
		if ch.Kind != chain.KindEVM {
			results[i] = FundsChain{ChainID: ch.ID, Name: ch.Name, Tag: ch.Tag, Assets: []FundsAsset{}}
			continue
		}
		wg.Add(1)
		go func(i int, ch chain.Chain) {
			defer wg.Done()
			results[i] = s.fundsForChain(r.Context(), ch, owner)
		}(i, ch)
	}
	wg.Wait()

	var grand, stables float64
	haveTotal, complete := false, true
	for _, fc := range results {
		if fc.TotalUSD != nil {
			grand += *fc.TotalUSD
			haveTotal = true
		} else if len(fc.Assets) > 0 {
			complete = false
		}
		for _, a := range fc.Assets {
			if a.IsStable && a.USD != nil {
				stables += *a.USD
			}
		}
	}

	resp := map[string]any{
		"chains":  results,
		"stables": stables,
	}
	if haveTotal && complete {
		resp["totalUsd"] = grand
	} else {
		resp["totalUsd"] = nil
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) fundsForChain(ctx context.Context, ch chain.Chain, owner common.Address) FundsChain {
	out := FundsChain{ChainID: ch.ID, Name: ch.Name, Tag: ch.Tag, Assets: []FundsAsset{}}

	nativeUSD, haveNativePrice := s.nativePriceUSD(ctx, ch)
	var total float64
	complete := true

	// Native balance.
	native := FundsAsset{Symbol: ch.NativeSymbol, Amount: "0", Decimals: 18, IsNative: true}
	if bal, err := withRetry(ctx, func() (*big.Int, error) { return s.nativeBalance(ctx, ch.ID, owner) }); err == nil && bal != nil {
		native.Amount = formatUnits(bal, 18)
		if haveNativePrice && nativeUSD > 0 {
			usd := parseFloat(native.Amount) * nativeUSD
			native.USD = &usd
			total += usd
		} else if bal.Sign() > 0 {
			complete = false // held, but not priced
		}
	} else {
		native.Unavailable = true
		complete = false
	}
	out.Assets = append(out.Assets, native)

	// Every configured stable is listed, a zero balance included: they are the
	// only ERC-20s whose price needs no derivation, and a stable that simply
	// vanished from the card on a failed read looked like an empty balance.
	for _, addr := range ch.Stables {
		asset := FundsAsset{Symbol: s.symbolFor(ctx, ch.ID, addr), Address: strings.ToLower(addr), Amount: "0", IsStable: true}
		var decimals int
		bal, err := withRetry(ctx, func() (*big.Int, error) {
			b, d, err := s.tokenBalance(ctx, ch.ID, common.HexToAddress(addr), owner)
			decimals = d
			return b, err
		})
		if err != nil || bal == nil {
			asset.Unavailable = true
			complete = false
			out.Assets = append(out.Assets, asset)
			continue
		}
		asset.Decimals = decimals
		asset.Amount = formatUnits(bal, decimals)
		usd := parseFloat(asset.Amount)
		asset.USD = &usd
		total += usd
		out.Assets = append(out.Assets, asset)
	}

	sort.SliceStable(out.Assets, func(i, j int) bool {
		if out.Assets[i].IsNative != out.Assets[j].IsNative {
			return out.Assets[i].IsNative
		}
		return out.Assets[i].Symbol < out.Assets[j].Symbol
	})

	// A chain total missing a balance would understate it; unknown is honest.
	if complete {
		out.TotalUSD = &total
	}
	return out
}

// withRetry retries a balance read a few times. Public endpoints, Robinhood's
// single one especially, throttle bursts, and the funds card reads every chain
// at once.
func withRetry(ctx context.Context, read func() (*big.Int, error)) (*big.Int, error) {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		var v *big.Int
		if v, err = read(); err == nil {
			return v, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(300*(attempt+1)) * time.Millisecond):
		}
	}
	return nil, err
}

func (s *Server) nativeBalance(ctx context.Context, chainID chain.ID, owner common.Address) (*big.Int, error) {
	var out hexutil.Big
	err := s.pool.Call(ctx, chainID, 0, &out, "eth_getBalance", owner, "latest")
	if err != nil {
		return nil, err
	}
	return (*big.Int)(&out), nil
}

func (s *Server) tokenBalance(ctx context.Context, chainID chain.ID, token, owner common.Address) (*big.Int, int, error) {
	data, err := balanceOf.Pack("balanceOf", owner)
	if err != nil {
		return nil, 0, err
	}
	raw, err := s.pool.EVM(chainID).CallContract(ctx, token, data, nil, nil)
	if err != nil {
		return nil, 0, err
	}
	vals, err := balanceOf.Unpack("balanceOf", raw)
	if err != nil || len(vals) == 0 {
		return nil, 0, fmt.Errorf("api: decode balance of %s: %v", token.Hex(), err)
	}
	bal, ok := vals[0].(*big.Int)
	if !ok {
		return nil, 0, fmt.Errorf("api: balance of %s is not a number", token.Hex())
	}

	decimals := 18
	if rec, err := s.store.GetToken(ctx, chainID, token.Hex()); err == nil && rec.Decimals != nil {
		decimals = *rec.Decimals
	} else if d, err := s.readDecimals(ctx, chainID, token); err == nil {
		decimals = d
	}
	return bal, decimals, nil
}

func (s *Server) readDecimals(ctx context.Context, chainID chain.ID, token common.Address) (int, error) {
	data, err := balanceOf.Pack("decimals")
	if err != nil {
		return 0, err
	}
	raw, err := s.pool.EVM(chainID).CallContract(ctx, token, data, nil, nil)
	if err != nil {
		return 0, err
	}
	vals, err := balanceOf.Unpack("decimals", raw)
	if err != nil || len(vals) == 0 {
		return 0, err
	}
	switch v := vals[0].(type) {
	case uint8:
		return int(v), nil
	case *big.Int:
		return int(v.Int64()), nil
	}
	return 18, nil
}

func (s *Server) symbolFor(ctx context.Context, chainID chain.ID, address string) string {
	if rec, err := s.store.GetToken(ctx, chainID, address); err == nil && rec.Symbol != "" {
		return rec.Symbol
	}
	return strings.ToUpper(address[:6])
}

// handlePortfolioPositions is not implemented: LP positions need per-wallet
// mint and burn indexing across DEX generations. Holdings are built from
// verified trades; see portfolio_trades.go.
func (s *Server) handlePortfolioPositions(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not_implemented",
		"LP position tracking needs per-wallet mint/burn indexing across DEX generations, "+
			"which is not built yet")
}

var _ = abi.ABI{}
