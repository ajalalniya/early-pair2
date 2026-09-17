package risk

import (
	"testing"

	"github.com/earlypair/backend/internal/config"
)

// The twelve pools from the design prototype, used as golden cases.
//
// A note on what is asserted. The prototype's own risk numbers are hand-authored
// design values, not the output of any scorer, so asserting an exact match would
// be asserting that we reverse-engineered someone's illustrative constants. What
// the scorer must reproduce is the *classification* those numbers imply — the
// band each pool lands in, and the ordering between the obviously-bad ones —
// because that is what actually gates a user's trade.
type fixture struct {
	name       string
	buyers     int
	sellers    int
	lpLock     string
	mintActive bool
	taxBuyBps  int
	taxSellBps int
	top10      float64
	holders    int
	verified   bool

	designScore int    // the prototype's hand-authored value, for reference
	wantBand    string // what the scorer must agree on
}

var fixtures = []fixture{
	{"GROK / WETH", 412, 188, "Locked 12mo", false, 0, 0, 8.4, 218, true, 12, BandSafe},
	{"BONKAI / SOL", 288, 41, "Unlocked", true, 500, 900, 41.2, 61, false, 74, BandDanger},
	{"AERO / USDC", 1840, 1207, "Locked 24mo", false, 0, 0, 4.1, 1902, true, 6, BandSafe},
	{"SUIPAD / SUI", 164, 122, "Locked 3mo", false, 100, 100, 22.7, 144, true, 38, BandWatch},
	{"HOODX / USDC", 508, 241, "Locked 6mo", false, 0, 100, 11.3, 512, true, 18, BandSafe},
	{"PEPE2 / WBNB", 1214, 3, "Unlocked", true, 1200, 2400, 68.9, 34, false, 91, BandDanger},
	{"ARBI / WETH", 342, 289, "Locked 9mo", false, 0, 0, 14.6, 388, true, 22, BandSafe},
	{"JITOX / SOL", 1021, 744, "Locked 18mo", false, 0, 0, 7.2, 962, true, 14, BandSafe},
	{"DEGEN9 / WETH", 286, 9, "Unlocked", true, 800, 800, 55.4, 22, false, 83, BandDanger},
	{"STETH / WETH", 3980, 3411, "Locked 36mo", false, 0, 0, 3.2, 5104, true, 4, BandSafe},
	{"TURBO / USDT", 221, 158, "Locked 1mo", false, 300, 300, 28.1, 208, true, 44, BandWatch},
	{"WAL / SUI", 398, 302, "Locked 12mo", false, 0, 0, 12.9, 421, true, 20, BandSafe},
}

func (f fixture) inputs() Inputs {
	return Inputs{
		Buyers1h:   &f.buyers,
		Sellers1h:  &f.sellers,
		LPLock:     f.lpLock,
		LockMonths: ParseLockMonths(f.lpLock),
		MintActive: &f.mintActive,
		TaxBuyBps:  &f.taxBuyBps,
		TaxSellBps: &f.taxSellBps,
		Top10Pct:   &f.top10,
		Holders:    &f.holders,
		Verified:   &f.verified,
	}
}

func TestGoldenFixtureBands(t *testing.T) {
	s := New(config.Default().Risk)

	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			got := s.Score(f.inputs())
			if got.Summary.Band != f.wantBand {
				t.Errorf("band = %s (score %d), want %s (design score %d)\nchecks: %+v",
					got.Summary.Band, got.Summary.Score, f.wantBand, f.designScore,
					got.Summary.Checks)
			}
			if !got.Complete {
				t.Errorf("fully specified fixture reported incomplete")
			}
			if len(got.Summary.Checks) != 8 {
				t.Errorf("got %d checks, want the 8 the UI renders", len(got.Summary.Checks))
			}
		})
	}
}

func TestHoneypotPatternDominatesVerdict(t *testing.T) {
	s := New(config.Default().Risk)
	// PEPE2: 1214 buyers, 3 sellers — a 404:1 ratio.
	var pepe fixture
	for _, f := range fixtures {
		if f.name == "PEPE2 / WBNB" {
			pepe = f
		}
	}
	got := s.Score(pepe.inputs())
	want := "1,214 addresses bought, only 3 ever sold — honeypot pattern. Trading is gated."
	if got.Summary.Verdict != want {
		t.Errorf("verdict = %q, want %q", got.Summary.Verdict, want)
	}
}

func TestUnknownInputsNeverLookSafe(t *testing.T) {
	s := New(config.Default().Risk)

	// A pool nothing has been established about must not come back as a clean
	// bill of health just because no check failed.
	got := s.Score(Inputs{})
	if got.Complete {
		t.Fatal("empty inputs reported as complete")
	}
	if got.Summary.Score != 0 {
		t.Errorf("unknown inputs contributed %d points; they must contribute none",
			got.Summary.Score)
	}
	for _, c := range got.Summary.Checks {
		if c.Known {
			t.Errorf("check %q claims to be established from empty inputs", c.Label)
		}
		if c.OK {
			t.Errorf("check %q passed without evidence", c.Label)
		}
	}
}

func TestSellSimulationRevertIsDecisive(t *testing.T) {
	s := New(config.Default().Risk)
	reverts := true
	zero := 0
	buyers, sellers := 100, 50
	holders := 500
	top := 5.0
	verified, mint := true, false

	in := Inputs{
		Buyers1h: &buyers, Sellers1h: &sellers,
		LPLock: "Locked 24mo", LockMonths: ParseLockMonths("Locked 24mo"),
		MintActive: &mint, TaxBuyBps: &zero, TaxSellBps: &zero,
		SellReverts: &reverts,
		Top10Pct:    &top, Holders: &holders, Verified: &verified,
	}
	got := s.Score(in)
	// Everything else about this pool looks impeccable; the reverting sell alone
	// has to be enough to gate it.
	if got.Summary.Band != BandWatch && got.Summary.Band != BandDanger {
		t.Errorf("honeypot scored %d (%s); a reverting sell must gate the pool",
			got.Summary.Score, got.Summary.Band)
	}
	if !s.Gated(got.Summary.Score, 0) {
		t.Errorf("honeypot with score %d was not gated at the default threshold",
			got.Summary.Score)
	}
}

func TestGatingNeverAppliesBelowThreshold(t *testing.T) {
	s := New(config.Default().Risk)
	if s.Gated(59, 60) {
		t.Error("score 59 gated at threshold 60")
	}
	if !s.Gated(60, 60) {
		t.Error("score 60 not gated at threshold 60")
	}
}

func TestParseLockMonths(t *testing.T) {
	cases := map[string]int{
		"Locked 12mo": 12,
		"Locked 1mo":  1,
		"Locked 36mo": 36,
	}
	for in, want := range cases {
		got := ParseLockMonths(in)
		if got == nil || *got != want {
			t.Errorf("ParseLockMonths(%q) = %v, want %d", in, got, want)
		}
	}
	if ParseLockMonths("Unlocked") != nil {
		t.Error("ParseLockMonths(Unlocked) should be nil")
	}
	if ParseLockMonths("Burned") != nil {
		t.Error("ParseLockMonths(Burned) should be nil")
	}
}

func TestBandBoundaries(t *testing.T) {
	cases := []struct {
		score int
		want  string
	}{
		{0, BandSafe}, {24, BandSafe}, {25, BandWatch},
		{59, BandWatch}, {60, BandDanger}, {100, BandDanger},
	}
	for _, c := range cases {
		if got := Band(c.score); got != c.want {
			t.Errorf("Band(%d) = %s, want %s", c.score, got, c.want)
		}
	}
}

// TestNoTradesIsNotAHoneypotTell reproduces what the live feed showed: brand-new
// BSC pools with 0 buyers and 0 sellers scored "SAFE 21" — 21 points from a
// "no exit observed" fail that nobody had had a chance to earn, and a SAFE
// label on a pool with no trading evidence at all.
func TestNoTradesIsNotAHoneypotTell(t *testing.T) {
	s := New(config.Default().Risk)
	zero, holders := 0, 500
	top, verified, mint := 5.0, true, false

	got := s.Score(Inputs{
		Buyers1h: &zero, Sellers1h: &zero,
		LPLock: "Burned", MintActive: &mint,
		TaxBuyBps: &zero, TaxSellBps: &zero,
		Top10Pct: &top, Holders: &holders, Verified: &verified,
	})

	if got.Summary.Score != 0 {
		t.Errorf("a pool with no trades scored %d; the absence of trades is not a failed check", got.Summary.Score)
	}
	if got.Summary.Band == BandSafe {
		t.Error("a pool nobody has traded was labelled SAFE; it has no exit evidence either way")
	}
	if got.Summary.Band != BandPending {
		t.Errorf("band = %s, want %s", got.Summary.Band, BandPending)
	}
	for _, c := range got.Summary.Checks {
		if (c.Label == "Unique sellers" || c.Label == "Buy / sell address ratio") && c.Value != "no trades yet" {
			t.Errorf("%s = %q, want %q", c.Label, c.Value, "no trades yet")
		}
	}
}

// TestPendingNeverLooksSafeButDangerStillShows: missing evidence can only add
// points, so a partial score that is already high must still read as DANGER.
func TestPendingNeverLooksSafeButDangerStillShows(t *testing.T) {
	s := New(config.Default().Risk)

	if got := s.Score(Inputs{}); got.Summary.Band != BandPending {
		t.Errorf("empty inputs banded %s, want %s", got.Summary.Band, BandPending)
	}

	mint, holders := true, 10
	top := 80.0
	got := s.Score(Inputs{LPLock: "Unlocked", MintActive: &mint, Top10Pct: &top, Holders: &holders})
	if got.Summary.Complete {
		t.Fatal("partial inputs reported complete")
	}
	if got.Summary.Band != BandDanger {
		t.Errorf("partial evidence already scoring %d banded %s; it must still read %s",
			got.Summary.Score, got.Summary.Band, BandDanger)
	}
}

// TestUnavailableChecksDoNotHoldAPoolPending: a check with no probe in this
// deployment will never fill in, so it must not keep every pool out of SAFE
// forever — but it has to be named, so a SAFE band never implies it ran.
func TestUnavailableChecksDoNotHoldAPoolPending(t *testing.T) {
	s := New(config.Default().Risk)
	buyers, sellers, holders := 300, 250, 800
	top, mint := 6.0, false

	got := s.Score(Inputs{
		Buyers1h: &buyers, Sellers1h: &sellers,
		LPLock: "Burned", MintActive: &mint,
		Top10Pct: &top, Holders: &holders,
		TaxProbeUnavailable:    true,
		VerifyProbeUnavailable: true,
	})

	if !got.Summary.Complete {
		t.Error("a pool with every available check established was reported incomplete")
	}
	if got.Summary.Band != BandSafe {
		t.Errorf("band = %s, want %s", got.Summary.Band, BandSafe)
	}
	if len(got.Summary.Unavailable) != 2 {
		t.Errorf("unavailable = %v, want the tax and verification checks named", got.Summary.Unavailable)
	}
	for _, c := range got.Summary.Checks {
		if c.Label == "Buy / sell tax" && (c.OK || c.Known || c.Value != "not available") {
			t.Errorf("unavailable tax check rendered as %+v", c)
		}
	}
}

// TestConcentratedLiquidityLockIsUnavailable: v3/v4 liquidity lives in NFT
// positions with no fungible LP token to read a lock from. Treating the check as
// pending would hold every such pool in PENDING forever.
func TestConcentratedLiquidityLockIsUnavailable(t *testing.T) {
	s := New(config.Default().Risk)
	buyers, sellers, holders := 200, 150, 600
	top, mint := 8.0, false

	got := s.Score(Inputs{
		Buyers1h: &buyers, Sellers1h: &sellers,
		MintActive: &mint, Top10Pct: &top, Holders: &holders,
		TaxProbeUnavailable: true, VerifyProbeUnavailable: true,
		LPLockUnavailable: true,
	})
	if !got.Summary.Complete || got.Summary.Band != BandSafe {
		t.Errorf("band=%s complete=%v; an otherwise clean CLMM pool must not be stuck pending",
			got.Summary.Band, got.Summary.Complete)
	}
}

// TestCustomHookIsNamed: a hook can take fees or block sells on every swap and
// nothing here audits it, so a SAFE band on a hooked pool must carry the caveat.
func TestCustomHookIsNamed(t *testing.T) {
	s := New(config.Default().Risk)
	buyers, sellers, holders := 200, 150, 600
	top, mint := 8.0, false

	got := s.Score(Inputs{
		Buyers1h: &buyers, Sellers1h: &sellers,
		MintActive: &mint, Top10Pct: &top, Holders: &holders,
		TaxProbeUnavailable: true, VerifyProbeUnavailable: true, LPLockUnavailable: true,
		CustomHook: "0x00000000000000000000000000000000deadbec0",
	})
	found := false
	for _, u := range got.Summary.Unavailable {
		if u == HookCaveat {
			found = true
		}
	}
	if !found {
		t.Errorf("unavailable = %v; a hooked pool must name the unaudited hook", got.Summary.Unavailable)
	}
}
