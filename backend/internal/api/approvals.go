package api

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/uniswapv4"
)

// Approvals, in one place.
//
// Every router this service builds swaps for needs permission to move the
// token being paid, and each asks for it differently: the v2 router and v3's
// SwapRouter02 take a plain allowance, v4's Universal Router pulls through
// Permit2 and so needs two. The swap builders, the approve-ahead endpoint and
// the portfolio's "ready to sell" flag all ask the same question, so they share
// one answer rather than three copies of the Permit2 rules.

// sellReadyFor is how long an approval must stay valid for a holding to count
// as ready to sell: long enough that "ready" does not lapse between reading it
// and clicking Sell.
const sellReadyFor = time.Hour

// approvalSteps lists the approvals owner still needs before token can be spent
// by the router that trades pool p, for at least need. Each approval it builds
// covers the wallet's whole balance, so the next trade in the token needs none.
//
// An allowance that cannot be read is skipped and reported in the error, with
// the steps that could be established still returned: a swap builder can go on
// and let simulation catch a shortfall, while the "ready" flag must treat an
// unreadable allowance as unknown rather than as granted.
func (s *Server) approvalSteps(ctx context.Context, ch chain.Chain, p chain.Pool, owner common.Address,
	token *chain.Token, need *big.Int, validUntil time.Time) ([]UnsignedTx, error) {
	if token == nil || chain.IsNative(token.Address) {
		return nil, nil // the native currency is sent as value; nothing to approve
	}
	tokenAddr := common.HexToAddress(token.Address)
	grant := approvalAmount(ctx, s, ch, tokenAddr, owner, need)

	switch {
	case isV4Pool(p):
		if ch.UniswapV4 == nil || ch.UniswapV4.Permit2 == "" || ch.UniswapV4.UniversalRouter == "" {
			return nil, errors.New("Uniswap v4 trading is not configured for this chain")
		}
		permit2 := common.HexToAddress(ch.UniswapV4.Permit2)
		router := common.HexToAddress(ch.UniswapV4.UniversalRouter)
		var steps []UnsignedTx
		var readErr error

		if current, err := s.erc20Allowance(ctx, ch.ID, tokenAddr, owner, permit2); err != nil {
			readErr = err
		} else if current.Cmp(need) < 0 {
			approve, _ := erc20Mini.Pack("approve", permit2, grant)
			steps = append(steps, UnsignedTx{
				ChainID: ch.ID, To: tokenAddr.Hex(), Data: hexutil.Encode(approve), Value: "0",
				Kind: "approve", Label: approvalLabel("Approve %s for Permit2", token, grant, need),
			})
		}

		amount, expiration, err := uniswapv4.Permit2Allowance(ctx, s.pool.EVM(ch.ID), permit2, owner, tokenAddr, router)
		if err != nil {
			readErr = errors.Join(readErr, err)
		}
		if err != nil || amount.Cmp(need) < 0 || expiration < uint64(validUntil.Unix()) {
			// Permit2 allowances expire; 30 days is Uniswap's own default, and it
			// bounds how long a forgotten approval stays usable.
			until := time.Now().Add(permit2Validity)
			data, err := uniswapv4.EncodePermit2Approve(tokenAddr, router, capUint160(grant), uint64(until.Unix()))
			if err != nil {
				return nil, fmt.Errorf("could not encode the Permit2 approval: %w", err)
			}
			steps = append(steps, UnsignedTx{
				ChainID: ch.ID, To: permit2.Hex(), Data: hexutil.Encode(data), Value: "0",
				Kind: "approve", Label: approvalLabel("Allow the Uniswap router to spend %s via Permit2", token, grant, need),
			})
		}
		return steps, readErr

	case isV3Pool(ch, p):
		if ch.UniswapV3 == nil || ch.UniswapV3.SwapRouter == "" {
			return nil, fmt.Errorf("%s trading is not configured for this chain", p.Dex)
		}
		return s.plainApproval(ctx, ch, token, owner, common.HexToAddress(ch.UniswapV3.SwapRouter), need, grant,
			"Approve %s for the Uniswap router")

	case p.Model == chain.ModelV2:
		if ch.RouterV2 == "" {
			return nil, errors.New("no v2 router is configured for this chain")
		}
		return s.plainApproval(ctx, ch, token, owner, common.HexToAddress(ch.RouterV2), need, grant,
			"Approve %s for the router")
	}
	return nil, fmt.Errorf("trading %s pools is not supported", p.Dex)
}

// plainApproval is the one-allowance case: the router pulls the token itself.
func (s *Server) plainApproval(ctx context.Context, ch chain.Chain, token *chain.Token, owner, spender common.Address,
	need, grant *big.Int, label string) ([]UnsignedTx, error) {
	tokenAddr := common.HexToAddress(token.Address)
	current, err := s.erc20Allowance(ctx, ch.ID, tokenAddr, owner, spender)
	if err != nil {
		return nil, err
	}
	if current.Cmp(need) >= 0 {
		return nil, nil
	}
	approve, _ := erc20Mini.Pack("approve", spender, grant)
	return []UnsignedTx{{
		ChainID: ch.ID, To: tokenAddr.Hex(), Data: hexutil.Encode(approve), Value: "0",
		Kind: "approve", Label: approvalLabel(label, token, grant, need),
	}}, nil
}

func (s *Server) erc20Allowance(ctx context.Context, chainID chain.ID, token, owner, spender common.Address) (*big.Int, error) {
	data, err := erc20Mini.Pack("allowance", owner, spender)
	if err != nil {
		return nil, err
	}
	out, err := s.pool.EVM(chainID).CallContract(ctx, token, data, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("allowance of %s could not be read: %w", token.Hex(), err)
	}
	vals, err := erc20Mini.Unpack("allowance", out)
	if err != nil || len(vals) == 0 {
		return nil, fmt.Errorf("allowance of %s could not be decoded", token.Hex())
	}
	current, ok := vals[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("allowance of %s could not be decoded", token.Hex())
	}
	return current, nil
}

// ------------------------------------------------------------------ approve ahead

type approveRequest struct {
	ChainID chain.ID `json:"chainId"`
	Pool    string   `json:"pool"`
	From    string   `json:"from"`
	// Side is the trade being prepared: "sell" (the default) approves the
	// token held, "buy" approves what pays for it.
	Side string `json:"side,omitempty"`
}

// handleApprove builds only the approvals a trade will need, so they can be
// signed ahead of time and the trade itself is then a single transaction —
// what matters when the moment to sell a new pair is measured in seconds.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	var req approveRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.Side == "" {
		req.Side = "sell"
	}
	if req.Side != "sell" && req.Side != "buy" {
		writeError(w, http.StatusBadRequest, "bad_side", `side must be "sell" or "buy"`)
		return
	}
	if !common.IsHexAddress(req.From) {
		writeError(w, http.StatusBadRequest, "bad_from", "from must be a wallet address")
		return
	}
	ch, ok := s.reg.Get(req.ChainID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown_chain", "no such chain")
		return
	}
	p, err := s.store.GetPool(r.Context(), req.ChainID, strings.ToLower(req.Pool))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if p.Token0 == nil || p.Token1 == nil || p.Token0.Symbol == "" || p.Token1.Symbol == "" {
		writeError(w, http.StatusConflict, "not_enriched", "this pool's tokens are not resolved yet; try again in a moment")
		return
	}

	token, _, _, _ := tradeSides(chain.Orient(ch, p.Token0, p.Token1), req.Side, nil, nil)
	owner := common.HexToAddress(req.From)
	ctx := r.Context()

	if chain.IsNative(token.Address) {
		writeJSON(w, http.StatusOK, map[string]any{"steps": []UnsignedTx{}, "approved": true,
			"note": fmt.Sprintf("%s is the native currency and needs no approval", token.Symbol)})
		return
	}
	balance, _, err := s.tokenBalance(ctx, ch.ID, common.HexToAddress(token.Address), owner)
	if err != nil {
		writeError(w, http.StatusBadGateway, "balance_unavailable", "this wallet's balance could not be read; try again")
		return
	}
	if balance.Sign() == 0 {
		writeError(w, http.StatusUnprocessableEntity, "nothing_to_approve",
			fmt.Sprintf("this wallet holds no %s to approve", token.Symbol))
		return
	}

	steps, err := s.approvalSteps(ctx, ch, p, owner, token, balance, time.Now().Add(sellReadyFor))
	if err != nil {
		// Unlike a swap, there is no simulation after this to catch a guess:
		// an unreadable allowance is an error, not a pass.
		s.log.Warn("approve-ahead could not read allowances", "pool", p.Address, "error", err)
		writeError(w, http.StatusBadGateway, "allowance_unavailable", "the current approvals could not be read; try again")
		return
	}
	if steps == nil {
		steps = []UnsignedTx{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"steps":     steps,
		"approved":  len(steps) == 0,
		"expiresAt": time.Now().Add(5 * time.Minute),
		"note":      "sign these in order with your own wallet; this service holds no keys and cannot sign",
	})
}
