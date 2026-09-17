// Package risk turns on-chain facts into the 0-100 fraud score and the eight
// PASS/FAIL checks the UI renders.
//
// Two rules shape everything here. First, a score is never produced from a
// guess: an input we have not established is reported as unknown and adds
// nothing, so a pool that has simply not been probed yet cannot look safe.
// Second, every score keeps the inputs and weights that produced it, so any
// verdict can be explained to a user and re-run against new weights without
// re-reading the chain.
package risk

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/config"
)

// Bands. SAFE, WATCH and DANGER are the design's three states. PENDING is
// what a low score earns while some check has not been established yet: a
// score that is low only because evidence is missing is not a clean bill of
// health, and labelling it SAFE is exactly the mistake this package exists to
// prevent. WATCH and DANGER need no such guard — missing evidence can only ever
// add points, never remove them, so a partial score that is already high is a
// floor, not a guess.
const (
	BandPending = "PENDING"
	BandSafe    = "SAFE"
	BandWatch   = "WATCH"
	BandDanger  = "DANGER"
)

// Inputs are the facts the scorer reasons over. Every pointer field is
// three-valued: nil means "not established", which is not the same as false.
type Inputs struct {
	Buyers1h  *int `json:"buyers1h"`
	Sellers1h *int `json:"sellers1h"`

	// LPLock is a human string: "Locked 12mo", "Burned", "Unlocked", or empty
	// when the locker set has not been checked.
	LPLock string `json:"lpLock"`
	// LockMonths is parsed out of LPLock when present; a one-month lock on a
	// brand-new pool is a materially different fact from a two-year one.
	LockMonths *int `json:"lockMonths"`

	MintActive *bool `json:"mintActive"`

	TaxBuyBps   *int  `json:"taxBuyBps"`
	TaxSellBps  *int  `json:"taxSellBps"`
	SellReverts *bool `json:"sellReverts"` // honeypot simulation outcome

	Top10Pct *float64 `json:"top10Pct"`
	Holders  *int     `json:"holders"`
	Verified *bool    `json:"verified"`

	// TaxProbeUnavailable and VerifyProbeUnavailable mark checks that cannot be
	// established at all in this deployment — no simulation probe exists, or no
	// explorer key is configured for the chain. They are reported as "not
	// available" instead of "not checked yet": waiting will never fill them in,
	// so letting them hold a pool in PENDING would keep every pool there forever.
	TaxProbeUnavailable    bool `json:"taxProbeUnavailable,omitempty"`
	VerifyProbeUnavailable bool `json:"verifyProbeUnavailable,omitempty"`
	// LPLockUnavailable is set for concentrated-liquidity pools (Uniswap v3/v4
	// and forks), where liquidity is held as per-owner NFT positions rather than
	// a fungible LP token whose distribution could be read. The lock check has
	// nothing to measure there, so it is "not available", not pending.
	LPLockUnavailable bool `json:"lpLockUnavailable,omitempty"`

	// PoolFeeBps is the pool's own static LP fee, when known. Uniswap v4 lets a
	// pool's creator set any fee up to 100%, and on Robinhood Chain two thirds
	// of new v4 pools were created at 10-100% — verified against the fee their
	// swaps actually charged (81%, 84%, 42%...). The creator, as the liquidity
	// provider, keeps that share of every buy and every sell. Economically it is
	// a trade tax charged by the pool rather than the token, and it is read
	// straight from the chain, so unlike token tax it can be established.
	PoolFeeBps *int `json:"poolFeeBps,omitempty"`

	// CustomHook is a Uniswap v4 pool's hook contract. A hook runs arbitrary
	// code on every swap: it can take a fee or make sells revert, and none of
	// that is visible from the token contract the other checks read. Nothing
	// here audits hook code, so its presence is named on the summary rather
	// than silently left out of a SAFE band.
	CustomHook string `json:"customHook,omitempty"`
}

// HookCaveat is the entry added to RiskSummary.Unavailable for a pool with a
// custom v4 hook.
const HookCaveat = "Custom v4 hook (not audited)"

const (
	// normalPoolFeeBps is the highest standard AMM fee tier (1%). v2 and v3
	// factories cannot exceed it; only v4 pools can.
	normalPoolFeeBps = 100
	// poolFeeExtremeBps is where a pool fee stops being a steep tier and becomes
	// the trade itself: at 25% a round trip already loses over 43%.
	poolFeeExtremeBps = 2500
)

// Result is the scorer's full output.
type Result struct {
	Summary chain.RiskSummary
	// Complete reports whether every input was established. The API surfaces it
	// so a partially-probed pool is not mistaken for a cleared one.
	Complete bool
	Inputs   Inputs
}

// Scorer holds the weights. They live in config so a newly observed rug pattern
// can be reweighted without a deploy.
type Scorer struct {
	w config.RiskWeights
}

func New(w config.RiskWeights) *Scorer { return &Scorer{w: w} }

func (s *Scorer) Weights() config.RiskWeights { return s.w }

// Score evaluates the inputs.
func (s *Scorer) Score(in Inputs) Result {
	var (
		points      int
		checks      []chain.RiskCheck
		pending     int
		unavailable []string
	)

	add := func(c chain.RiskCheck, penalty int) {
		checks = append(checks, c)
		points += penalty
	}
	// unknownWith reports a check that is not established yet. It is not-OK so
	// it can never read as a pass, Known is false so the UI can tell "checked
	// and failed" from "not checked", and it adds nothing to the score.
	unknownWith := func(label, value string) chain.RiskCheck {
		pending++
		return chain.RiskCheck{Label: label, Value: value, OK: false, Known: false}
	}
	unknown := func(label string) chain.RiskCheck { return unknownWith(label, "not checked yet") }
	notAvailable := func(label string) chain.RiskCheck {
		unavailable = append(unavailable, label)
		return chain.RiskCheck{Label: label, Value: "not available", OK: false, Known: false}
	}

	// A pool nobody has traded yet says nothing about whether exits work. Zero
	// sellers alongside zero buyers is an absence of evidence, not the "no exit
	// observed" honeypot tell, and scoring it as one penalised every brand-new
	// pool for simply being new.
	noTrades := in.Buyers1h != nil && in.Sellers1h != nil && *in.Buyers1h == 0 && *in.Sellers1h == 0

	// 1. Unique sellers. A pool nobody has ever sold out of is the single
	//    loudest honeypot tell, which is why it leads.
	switch {
	case in.Sellers1h == nil:
		add(unknown("Unique sellers"), 0)
	case noTrades:
		add(unknownWith("Unique sellers", "no trades yet"), 0)
	default:
		n := *in.Sellers1h
		value := fmt.Sprintf("%s addresses", comma(n))
		if n == 0 {
			value = "0 — no exit observed"
		}
		ok := n > 20
		penalty := 0
		if !ok {
			penalty = s.w.SellersThin
			if n == 0 {
				penalty = s.w.SellersThin + s.w.SellersThin/2
			}
		}
		add(chain.RiskCheck{Label: "Unique sellers", Value: value, OK: ok, Known: true}, penalty)
	}

	// 2. Buyer-to-seller address ratio. Counting addresses rather than swaps is
	//    what makes this hard to fake with a single looping bot.
	switch {
	case in.Buyers1h == nil || in.Sellers1h == nil:
		add(unknown("Buy / sell address ratio"), 0)
	case noTrades:
		add(unknownWith("Buy / sell address ratio", "no trades yet"), 0)
	default:
		b, sl := *in.Buyers1h, *in.Sellers1h
		ratio := math.Inf(1)
		value := "∞ : 1"
		if sl > 0 {
			ratio = float64(b) / float64(sl)
			value = fmt.Sprintf("%.1f : 1", ratio)
		}
		ok := ratio < 8
		penalty := 0
		if !ok {
			penalty = s.w.RatioHigh
		}
		add(chain.RiskCheck{Label: "Buy / sell address ratio", Value: value, OK: ok, Known: true}, penalty)
	}

	// 3. LP lock.
	switch {
	case in.LPLock == "" && in.LPLockUnavailable:
		add(notAvailable("LP lock"), 0)
	case in.LPLock == "":
		add(unknown("LP lock"), 0)
	default:
		locked := strings.HasPrefix(in.LPLock, "Locked") || in.LPLock == "Burned"
		penalty := 0
		switch {
		case !locked:
			penalty = s.w.LPUnlocked
		case in.LockMonths != nil && *in.LockMonths < 6:
			// Locked, but not for long. The check still passes — the lock is
			// real — while the score reflects that it expires soon.
			penalty = s.w.LPShortLock
		}
		add(chain.RiskCheck{Label: "LP lock", Value: in.LPLock, OK: locked, Known: true}, penalty)
	}

	// 4. Mint authority.
	if in.MintActive == nil {
		add(unknown("Mint authority"), 0)
	} else {
		active := *in.MintActive
		value := "Revoked"
		if active {
			value = "Active — owner can mint"
		}
		penalty := 0
		if active {
			penalty = s.w.MintActive
		}
		add(chain.RiskCheck{Label: "Mint authority", Value: value, OK: !active, Known: true}, penalty)
	}

	// 5. Buy/sell tax, carrying the honeypot simulation. A sell that reverts in
	//    simulation is not a tax problem, it is the whole answer, so it takes
	//    over this row and dominates the score.
	extremeFee := in.PoolFeeBps != nil && *in.PoolFeeBps >= poolFeeExtremeBps
	switch {
	case in.PoolFeeBps != nil && *in.PoolFeeBps > normalPoolFeeBps &&
		!(in.SellReverts != nil && *in.SellReverts):
		// A pool fee above the standard tiers is a tax on every trade, and a
		// known one. Token-level tax may still exist on top, but this alone is
		// enough to fail the check.
		fee := *in.PoolFeeBps
		penalty := s.w.TaxNonZero
		switch {
		case extremeFee:
			penalty = s.w.PoolFeeExtreme
		case fee >= 1000:
			penalty = s.w.TaxPunitive
		}
		add(chain.RiskCheck{
			Label: "Buy / sell tax",
			Value: fmt.Sprintf("Pool fee %s %% on every trade", bpsPct(fee)),
			OK:    false, Known: true,
		}, penalty)

	case in.SellReverts != nil && *in.SellReverts:
		add(chain.RiskCheck{
			Label: "Buy / sell tax",
			Value: "Sell simulation reverted — honeypot",
			OK:    false, Known: true,
		}, s.w.HoneypotSimFailed)

	case (in.TaxBuyBps == nil || in.TaxSellBps == nil) && in.TaxProbeUnavailable:
		add(notAvailable("Buy / sell tax"), 0)

	case in.TaxBuyBps == nil || in.TaxSellBps == nil:
		add(unknown("Buy / sell tax"), 0)

	default:
		buy, sell := *in.TaxBuyBps, *in.TaxSellBps
		value := fmt.Sprintf("%s / %s %%", bpsPct(buy), bpsPct(sell))
		ok := buy == 0 && sell == 0
		penalty := 0
		switch {
		case ok:
		case buy >= 1000 || sell >= 1000: // 10%+ is extractive, not a fee
			penalty = s.w.TaxPunitive
		default:
			penalty = s.w.TaxNonZero
		}
		add(chain.RiskCheck{Label: "Buy / sell tax", Value: value, OK: ok, Known: true}, penalty)
	}

	// 6. Top-10 concentration, graded: 26% and 70% are not the same danger.
	if in.Top10Pct == nil {
		add(unknown("Top 10 holders"), 0)
	} else {
		pct := *in.Top10Pct
		ok := pct < 25
		penalty := 0
		switch {
		case pct > 60:
			penalty = s.w.TopHeavyExtreme
		case pct > 40:
			penalty = s.w.TopHeavySevere
		case pct >= 25:
			penalty = s.w.TopHeavy
		case pct > 20:
			penalty = s.w.TopHeavyMild
		}
		add(chain.RiskCheck{
			Label: "Top 10 holders",
			Value: fmt.Sprintf("%.1f %% of supply", pct),
			OK:    ok, Known: true,
		}, penalty)
	}

	// 7. Holder count.
	if in.Holders == nil {
		add(unknown("Holders"), 0)
	} else {
		n := *in.Holders
		ok := n > 100
		penalty := 0
		if !ok {
			penalty = s.w.FewHolders
		}
		add(chain.RiskCheck{Label: "Holders", Value: comma(n), OK: ok, Known: true}, penalty)
	}

	// 8. Source verification.
	switch {
	case in.Verified == nil && in.VerifyProbeUnavailable:
		add(notAvailable("Contract verified"), 0)
	case in.Verified == nil:
		add(unknown("Contract verified"), 0)
	default:
		v := *in.Verified
		value := "Verified"
		if !v {
			value = "Unverified source"
		}
		penalty := 0
		if !v {
			penalty = s.w.Unverified
		}
		add(chain.RiskCheck{Label: "Contract verified", Value: value, OK: v, Known: true}, penalty)
	}

	score := points
	if score > 100 {
		score = 100
	}
	if score < 0 {
		score = 0
	}

	if in.CustomHook != "" {
		unavailable = append(unavailable, HookCaveat)
	}

	complete := pending == 0
	band := Band(score)
	verdict := s.verdict(score, in)
	if !complete && band == BandSafe {
		band = BandPending
		verdict = fmt.Sprintf(
			"Not enough evidence yet: %d of %d checks are still pending. Treat this pool as unscored.",
			pending, len(checks))
	}

	return Result{
		Summary: chain.RiskSummary{
			Score:       score,
			Band:        band,
			Verdict:     verdict,
			Checks:      checks,
			ScoredAt:    time.Now().UTC(),
			Complete:    complete,
			Unavailable: unavailable,
		},
		Complete: complete,
		Inputs:   in,
	}
}

// Band maps a score to SAFE, WATCH or DANGER by score alone. Score applies the
// PENDING override on top, because that needs to know what is missing.
func Band(score int) string {
	switch {
	case score < 25:
		return BandSafe
	case score < 60:
		return BandWatch
	default:
		return BandDanger
	}
}

// verdict renders the plain-language sentence, in the UI's priority order.
func (s *Scorer) verdict(score int, in Inputs) string {
	// Said plainly because the band alone undersells it: at this fee most of any
	// buy is gone before the user holds the token, and most of the sell after.
	if in.PoolFeeBps != nil && *in.PoolFeeBps >= poolFeeExtremeBps {
		return fmt.Sprintf(
			"This pool charges %s %% on every trade — most of any buy or sell goes to the pool's creator. Trading is gated.",
			bpsPct(*in.PoolFeeBps))
	}
	if in.Buyers1h != nil && in.Sellers1h != nil {
		b, sl := *in.Buyers1h, *in.Sellers1h
		if b > 0 && (sl == 0 || float64(b)/float64(sl) > 20) {
			return fmt.Sprintf(
				"%s addresses bought, only %s ever sold — honeypot pattern. Trading is gated.",
				comma(b), comma(sl))
		}
	}
	switch {
	case score >= s.w.GateThreshold:
		return "High probability of rug or honeypot. Trading is gated."
	case score >= 25:
		return "Mixed signals — seller base is thin relative to buyers. Size positions carefully."
	default:
		return "No critical failures detected. Buyer and seller address counts are balanced."
	}
}

// Gated reports whether a buy should be refused at the caller's threshold.
// Exits are never gated anywhere in this service.
func (s *Scorer) Gated(score int, threshold int) bool {
	if threshold <= 0 {
		threshold = s.w.GateThreshold
	}
	return score >= threshold
}

// ParseLockMonths pulls a month count out of a lock string like "Locked 12mo".
func ParseLockMonths(lock string) *int {
	f := strings.Fields(lock)
	for _, part := range f {
		part = strings.TrimSuffix(part, "mo")
		var n int
		if _, err := fmt.Sscanf(part, "%d", &n); err == nil && n > 0 && n < 1200 {
			v := n
			return &v
		}
	}
	return nil
}

func bpsPct(bps int) string {
	if bps%100 == 0 {
		return fmt.Sprintf("%d", bps/100)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", float64(bps)/100), "0"), ".")
}

func comma(n int) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 {
		return "-" + comma(-n)
	}
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, d := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, d)
	}
	return string(out)
}
