package enrich

import (
	"context"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/risk"
	"github.com/earlypair/backend/internal/rpcpool"
	"github.com/earlypair/backend/internal/store"
)

// riskInputs gathers the safety facts. Every probe returns a pointer, and a nil
// pointer means "not established" — which the scorer treats as contributing
// nothing rather than as a pass. A pool that has not been probed can therefore
// never look clean; it looks unfinished, which is the truth.
func (e *Enricher) riskInputs(ctx context.Context, evm *rpcpool.EVM, ch chain.Chain, p chain.Pool) risk.Inputs {
	ex, hasExplorer := e.explorers[string(ch.ID)]
	in := risk.Inputs{
		Buyers1h:  p.Buyers1h,
		Sellers1h: p.Sellers1h,
		// No tax/honeypot simulation exists yet (see below), and verification
		// needs an explorer key. Marking them unavailable keeps them from pinning
		// every pool in PENDING while still naming them on the summary.
		TaxProbeUnavailable:    true,
		VerifyProbeUnavailable: !hasExplorer || ex.API == "" || ex.APIKey == "",
		// Concentrated-liquidity positions are NFTs, not a fungible LP token, so
		// there is no distribution to read a lock from.
		LPLockUnavailable: p.Model == chain.ModelCLMM,
		CustomHook:        p.Hooks,
		PoolFeeBps:        p.FeeTierBps,
	}

	// The volatile side is the one being assessed: pairing against a stable or
	// the wrapped native tells you nothing about the stable.
	subject := e.subjectToken(ch, p)
	if subject == nil {
		return in
	}

	cached, cacheErr := e.store.GetToken(ctx, p.ChainID, subject.Address)
	rec := store.TokenRecord{ChainID: p.ChainID, Address: subject.Address}
	dirty := false

	// LP lock, from the LP token's own distribution. Only meaningful for
	// v2-style pools, where the pool contract is itself the LP token.
	if cacheErr == nil && cached.LPLock != "" {
		in.LPLock = cached.LPLock
	} else if p.Model != chain.ModelCLMM {
		if lock, ok := e.probeLPLock(ctx, evm, ch, p); ok {
			in.LPLock = lock
			rec.LPLock = lock
			dirty = true
		}
	}
	in.LockMonths = risk.ParseLockMonths(in.LPLock)

	// Mint authority.
	if cacheErr == nil && cached.MintActive != nil {
		in.MintActive = cached.MintActive
	} else if active, ok := e.probeMintAuthority(ctx, evm, subject.Address); ok {
		in.MintActive = &active
		rec.MintActive = &active
		dirty = true
	}

	// Holder distribution.
	switch {
	case cacheErr == nil && cached.Holders != nil && cached.Top10Pct != nil:
		in.Holders, in.Top10Pct = cached.Holders, cached.Top10Pct
	default:
		if holders, top10, ok := e.probeHolders(ctx, evm, ch, p, subject); ok {
			in.Holders, in.Top10Pct = &holders, &top10
			rec.Holders, rec.Top10Pct = &holders, &top10
			dirty = true
		}
	}

	// Source verification. Without an explorer key we report nothing rather
	// than assume either way.
	if cacheErr == nil && cached.Verified != nil {
		in.Verified = cached.Verified
	} else if v, ok := e.probeVerified(ctx, ch, subject.Address); ok {
		in.Verified = &v
		rec.Verified = &v
		dirty = true
	}

	// Buy/sell tax and the honeypot simulation are deliberately left
	// unestablished.
	//
	// Measuring them means simulating a buy and a sell against pending state
	// with eth_call state overrides, which several free endpoints do not
	// support at all and which cannot be verified correct without running it
	// against live chains. An unverified simulator would produce confident tax
	// percentages that might simply be wrong, and this is the one figure a user
	// is most likely to act on. Until it can be validated, these stay nil: the
	// two checks report "not checked yet", the summary is marked incomplete,
	// and the score reflects only what was actually observed.
	//
	// Anything already recorded by a previous probe is still read back here, so
	// the wiring is in place the moment the probe exists.
	if cacheErr == nil {
		in.TaxBuyBps, in.TaxSellBps = cached.TaxBuyBps, cached.TaxSellBps
		in.SellReverts = cached.Honeypot
	}

	if dirty {
		now := time.Now().UTC()
		rec.ProbedAt = &now
		if err := e.store.UpsertToken(ctx, rec); err != nil {
			e.log.Debug("could not persist probe results", "token", subject.Address, "error", err)
		}
	}
	return in
}

// subjectToken picks the side of the pair worth assessing.
func (e *Enricher) subjectToken(ch chain.Chain, p chain.Pool) *chain.Token {
	return chain.Orient(ch, p.Token0, p.Token1).Base
}

// probeLPLock reads where the LP tokens went. Burned to a dead address or held
// by a known locker counts as locked; anything else is a rug waiting to happen.
//
// The duration is not reported: reading a lock's unlock timestamp means knowing
// each locker's interface, and guessing a duration would be worse than omitting
// it, so the check reports "Locked" without a month count and the scorer treats
// an unknown duration as neither short nor long.
func (e *Enricher) probeLPLock(ctx context.Context, evm *rpcpool.EVM, ch chain.Chain, p chain.Pool) (string, bool) {
	lp := common.HexToAddress(p.Address)

	supplyData, err := erc20.Pack("totalSupply")
	if err != nil {
		return "", false
	}
	calls := []call3{{Target: lp, AllowFailure: true, CallData: supplyData}}

	sinks := append([]string{
		"0x000000000000000000000000000000000000dEaD",
		"0x0000000000000000000000000000000000000000",
	}, ch.LPLockers...)

	for _, holder := range sinks {
		data, err := erc20.Pack("balanceOf", common.HexToAddress(holder))
		if err != nil {
			return "", false
		}
		calls = append(calls, call3{Target: lp, AllowFailure: true, CallData: data})
	}

	results, err := e.multicall(ctx, evm, ch, calls)
	if err != nil || len(results) != len(calls) {
		return "", false
	}
	if !results[0].Success {
		return "", false
	}
	supply, ok := decodeBigInt(erc20, "totalSupply", results[0].ReturnData)
	if !ok || supply == nil || supply.Sign() == 0 {
		return "", false
	}

	burned := new(big.Int)
	locked := new(big.Int)
	for i, holder := range sinks {
		r := results[i+1]
		if !r.Success {
			continue
		}
		bal, ok := decodeBigInt(erc20, "balanceOf", r.ReturnData)
		if !ok || bal == nil {
			continue
		}
		if i < 2 {
			burned.Add(burned, bal)
		} else {
			locked.Add(locked, bal)
			_ = holder
		}
	}

	pctOf := func(v *big.Int) float64 {
		num := new(big.Float).SetInt(v)
		den := new(big.Float).SetInt(supply)
		f, _ := new(big.Float).Quo(num, den).Float64()
		return f * 100
	}

	switch {
	case pctOf(burned) >= 50:
		return "Burned", true
	case pctOf(locked) >= 50:
		return "Locked", true
	default:
		return "Unlocked", true
	}
}

// Selectors for the two mint signatures that cover almost every ERC-20 with an
// owner-callable supply switch.
var mintSelectors = [][]byte{
	{0x40, 0xc1, 0x0f, 0x19}, // mint(address,uint256)
	{0xa0, 0x71, 0x2d, 0x68}, // mint(uint256)
}

// probeMintAuthority reports whether supply can still be inflated: the bytecode
// contains a mint entrypoint *and* ownership has not been renounced. Either
// alone is not enough — a mint function nobody can call is inert, and an owner
// on a fixed-supply token cannot print.
func (e *Enricher) probeMintAuthority(ctx context.Context, evm *rpcpool.EVM, token string) (bool, bool) {
	code, err := evm.CodeAt(ctx, common.HexToAddress(token))
	if err != nil || len(code) == 0 {
		return false, false
	}

	hasMint := false
	for _, sel := range mintSelectors {
		if containsBytes(code, sel) {
			hasMint = true
			break
		}
	}
	if !hasMint {
		return false, true // no mint entrypoint at all: supply is fixed
	}

	data, err := erc20.Pack("owner")
	if err != nil {
		return false, false
	}
	out, err := evm.CallContract(ctx, common.HexToAddress(token), data, nil, nil)
	if err != nil {
		if rpcpool.IsExecutionReverted(err) {
			// No owner() accessor. A mint exists but we cannot establish who may
			// call it, so this stays unknown rather than becoming a guess.
			return false, false
		}
		return false, false
	}
	vals, err := erc20.Unpack("owner", out)
	if err != nil || len(vals) == 0 {
		return false, false
	}
	owner, _ := vals[0].(common.Address)
	renounced := owner == (common.Address{})
	return !renounced, true
}

// probeHolders reconstructs the holder set from the token's Transfer history.
//
// This is affordable precisely because the product only cares about new tokens:
// a pool minutes old has a handful of transfers. The sweep is capped, and when
// a token turns out to have more history than the cap allows, the result is
// reported as unknown rather than as a partial count that would understate
// concentration — the direction that would make a token look safer than it is.
func (e *Enricher) probeHolders(ctx context.Context, evm *rpcpool.EVM, ch chain.Chain, p chain.Pool, token *chain.Token) (int, float64, bool) {
	const maxLogs = 20_000
	// maxQueries bounds the sweep by cost as maxLogs bounds it by size. Block
	// counts are not time: a pool an hour old is 300 blocks of Ethereum but
	// 36,000 of Robinhood, dozens of log queries that a single 5 req/s endpoint
	// pays for out of the budget pool discovery runs on. Past the cap the
	// answer is unknown, as it is past maxLogs.
	const maxQueries = 6

	head := e.pool.FleetHead(p.ChainID)
	if head == 0 {
		return 0, 0, false
	}
	from := p.CreatedBlock
	if from == 0 || from > head {
		return 0, 0, false
	}

	eps := e.pool.Endpoints(p.ChainID, chain.HTTP)
	if len(eps) == 0 {
		return 0, 0, false
	}
	ep := eps[0]

	balances := map[common.Address]*big.Int{}
	logCount := 0
	target := common.HexToAddress(token.Address)

	for start, queries := from, 0; start <= head; queries++ {
		if queries >= maxQueries {
			return 0, 0, false
		}
		span := ep.LogRange()
		if span == 0 {
			span = 1
		}
		end := start + span - 1
		if end > head {
			end = head
		}

		logs, err := evm.FilterLogs(ctx, ep, rpcpool.FilterQuery{
			FromBlock: start, ToBlock: end,
			Addresses: []common.Address{target},
			Topics:    [][]common.Hash{{chain.TopicTransfer}},
		})
		if err != nil {
			if rpcpool.IsRangeTooLarge(err) {
				ep.ShrinkLogRange()
				continue
			}
			return 0, 0, false
		}

		logCount += len(logs)
		if logCount > maxLogs {
			// Too much history to reconstruct within budget.
			return 0, 0, false
		}

		for _, l := range logs {
			if l.Removed || len(l.Topics) < 3 || len(l.Data) < 32 {
				continue
			}
			fromAddr := common.BytesToAddress(l.Topics[1].Bytes())
			toAddr := common.BytesToAddress(l.Topics[2].Bytes())
			amount := new(big.Int).SetBytes(l.Data[:32])

			if fromAddr != (common.Address{}) {
				bal := balances[fromAddr]
				if bal == nil {
					bal = new(big.Int)
					balances[fromAddr] = bal
				}
				bal.Sub(bal, amount)
			}
			if toAddr != (common.Address{}) {
				bal := balances[toAddr]
				if bal == nil {
					bal = new(big.Int)
					balances[toAddr] = bal
				}
				bal.Add(bal, amount)
			}
		}
		start = end + 1
	}

	// Holders exclude the pool itself and the burn sinks: pooled liquidity is
	// not a holder, and counting it would flatter the concentration figure.
	excluded := map[common.Address]bool{
		common.HexToAddress(p.Address):                                    true,
		common.HexToAddress("0x000000000000000000000000000000000000dEaD"): true,
		common.Address{}: true,
	}

	total := new(big.Int)
	var positive []*big.Int
	holders := 0
	for addr, bal := range balances {
		if bal.Sign() <= 0 || excluded[addr] {
			continue
		}
		holders++
		total.Add(total, bal)
		positive = append(positive, bal)
	}
	if holders == 0 || total.Sign() == 0 {
		return 0, 0, false
	}

	// Top ten by balance.
	for i := 0; i < len(positive); i++ {
		for j := i + 1; j < len(positive); j++ {
			if positive[j].Cmp(positive[i]) > 0 {
				positive[i], positive[j] = positive[j], positive[i]
			}
		}
		if i >= 10 {
			break
		}
	}
	top := new(big.Int)
	for i := 0; i < len(positive) && i < 10; i++ {
		top.Add(top, positive[i])
	}

	num := new(big.Float).SetInt(top)
	den := new(big.Float).SetInt(total)
	pct, _ := new(big.Float).Quo(num, den).Float64()
	return holders, pct * 100, true
}

// probeVerified asks the chain's explorer whether the source is published.
// Without a configured API key this returns "not established" rather than
// assuming unverified, which would penalise every token on a chain we simply
// have no explorer credentials for.
func (e *Enricher) probeVerified(ctx context.Context, ch chain.Chain, token string) (bool, bool) {
	ex, ok := e.explorers[string(ch.ID)]
	if !ok || ex.API == "" || ex.APIKey == "" {
		return false, false
	}
	return e.explorerVerified(ctx, ex.API, ex.APIKey, token)
}

func containsBytes(haystack, needle []byte) bool {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
