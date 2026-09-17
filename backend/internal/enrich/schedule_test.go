package enrich

import (
	"testing"

	"github.com/earlypair/backend/internal/chain"
)

func pool(chainID chain.ID, addr string) chain.Pool {
	return chain.Pool{ChainID: chainID, Address: addr}
}

// TestScheduleNeverQueuesAPoolTwice is the starvation bug: the feeder re-offers
// every pool still needing work on each tick, and without this guard the queue
// filled with duplicates of old pools while new ones waited behind them.
func TestScheduleNeverQueuesAPoolTwice(t *testing.T) {
	s := newSchedule(8)
	p := pool("base", "0xaa")
	if !s.claim(p) {
		t.Fatal("first claim refused")
	}
	if s.claim(p) {
		t.Error("a pool already in flight was claimed again")
	}
	s.done(p)
	if !s.claim(p) {
		t.Error("a finished pool could not be claimed for its next pass")
	}
}

// TestScheduleCapsEachChain: a chain on one slow endpoint must not hold every
// worker and stall enrichment everywhere else.
func TestScheduleCapsEachChain(t *testing.T) {
	s := newSchedule(8) // cap of 4 per chain
	for i := 0; i < 4; i++ {
		if !s.claim(pool("rbn", string(rune('a'+i)))) {
			t.Fatalf("claim %d on rbn refused below the cap", i)
		}
	}
	if s.claim(pool("rbn", "e")) {
		t.Error("rbn took a fifth worker; its share is four")
	}
	if !s.claim(pool("eth", "x")) {
		t.Error("eth was blocked because rbn is busy")
	}
	s.done(pool("rbn", "a"))
	if !s.claim(pool("rbn", "e")) {
		t.Error("rbn could not use a worker freed by its own finished pool")
	}
}

func TestScheduleReleaseRestoresCapacity(t *testing.T) {
	s := newSchedule(4) // cap of 2 per chain
	a, b := pool("bsc", "a"), pool("bsc", "b")
	s.claim(a)
	s.claim(b)
	s.release(b) // queue was full; hand it back
	if !s.claim(pool("bsc", "c")) {
		t.Error("a released claim did not free its chain's share")
	}
}
