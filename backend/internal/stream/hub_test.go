package stream

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/config"
)

func testHub() *Hub {
	cfg := config.Default().Stream
	cfg.ReplayMaxFrames = 100
	return NewHub(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func subscribed(topics ...string) *Client {
	c := NewClient("test", 8)
	c.Subscribe(topics, nil)
	return c
}

// TestReplayIsLossless is the property the frontend's reconnect handling
// depends on: a client that says what it last saw gets exactly what it missed,
// in order, with nothing repeated.
func TestReplayIsLossless(t *testing.T) {
	h := testHub()
	c := subscribed(TopicPoolsNew)
	h.Add(c)

	for i := 0; i < 5; i++ {
		h.Publish(TopicPoolsNew, "eth", map[string]int{"n": i})
	}
	seenUpTo := h.Seq() - 2 // pretend the last two frames were missed

	missed, complete := h.Since(seenUpTo, c.Subscription())
	if !complete {
		t.Fatal("replay reported a gap for frames still inside the buffer")
	}
	if len(missed) != 2 {
		t.Fatalf("replayed %d frames, expected the 2 that were missed", len(missed))
	}
	if missed[0].Seq != seenUpTo+1 {
		t.Errorf("replay started at seq %d, expected %d", missed[0].Seq, seenUpTo+1)
	}
	for i := 1; i < len(missed); i++ {
		if missed[i].Seq <= missed[i-1].Seq {
			t.Error("replayed frames are not in sequence order")
		}
	}
}

// TestReplayReportsAGapItCannotFill matters more than the happy path: silently
// returning a partial history would let a client believe it is up to date when
// it has a hole in its feed.
func TestReplayReportsAGapItCannotFill(t *testing.T) {
	h := testHub()
	c := subscribed(TopicPoolsNew)

	for i := 0; i < 150; i++ { // more than ReplayMaxFrames
		h.Publish(TopicPoolsNew, "eth", i)
	}

	if _, complete := h.Since(1, c.Subscription()); complete {
		t.Error("a client that fell further behind than the buffer was told it was complete")
	}
}

// TestSlowClientIsDroppedNotBuffered protects the server: one stalled consumer
// must not be able to grow memory without bound.
func TestSlowClientIsDroppedNotBuffered(t *testing.T) {
	h := testHub()
	c := subscribed(TopicPoolsNew)
	h.Add(c)

	// Never drain c.Out(); the queue is 8 deep.
	for i := 0; i < 50; i++ {
		h.Publish(TopicPoolsNew, "eth", i)
	}

	if h.Clients() != 0 {
		t.Error("a client that never drained its queue was kept connected")
	}
	if !c.NeedsResync() {
		t.Error("the dropped client was not flagged for resync; it would reconnect with a silent hole")
	}
}

// TestSubscriptionFiltering checks that a client only receives what it asked
// for — both by topic and by chain.
func TestSubscriptionFiltering(t *testing.T) {
	h := testHub()

	c := NewClient("filtered", 16)
	c.Subscribe([]string{TopicPoolsNew}, []chain.ID{"base"})
	h.Add(c)

	h.Publish(TopicPoolsNew, "base", "wanted")
	h.Publish(TopicPoolsNew, "eth", "wrong chain")
	h.Publish(TopicHead, "base", "wrong topic")

	// Give the broadcast nothing to race: Publish is synchronous.
	var got []Frame
	for {
		select {
		case f := <-c.Out():
			got = append(got, f)
			continue
		default:
		}
		break
	}

	if len(got) != 1 {
		t.Fatalf("received %d frames, expected only the subscribed one: %+v", len(got), got)
	}
	if got[0].Data != "wanted" {
		t.Errorf("received %v, expected the base/pools.new frame", got[0].Data)
	}
}

// TestUnsubscribedClientReceivesNothing: a connection that has not subscribed
// is genuinely idle rather than receiving the whole firehose by default.
func TestUnsubscribedClientReceivesNothing(t *testing.T) {
	h := testHub()
	c := NewClient("silent", 8)
	h.Add(c)

	h.Publish(TopicPoolsNew, "eth", "x")

	select {
	case f := <-c.Out():
		t.Errorf("an unsubscribed client received %+v", f)
	default:
	}
}

// TestCoalescingKeepsTheNewestState: a busy pool must not flood clients, but
// what does arrive has to be the latest state, not the oldest.
func TestCoalescingKeepsTheNewestState(t *testing.T) {
	h := testHub()
	c := subscribed(TopicPoolsUpdate)
	h.Add(c)

	for i := 0; i < 20; i++ {
		h.PublishCoalesced(TopicPoolsUpdate, "eth:0xpool", "eth", i)
	}
	h.flushCoalesced()

	var got []Frame
	for {
		select {
		case f := <-c.Out():
			got = append(got, f)
			continue
		default:
		}
		break
	}

	if len(got) != 1 {
		t.Fatalf("coalescing emitted %d frames for one entity, expected 1", len(got))
	}
	if got[0].Data != 19 {
		t.Errorf("coalesced frame carried %v, expected the newest value 19", got[0].Data)
	}
}

func TestPruneDropsFramesOutsideTheWindow(t *testing.T) {
	cfg := config.Default().Stream
	cfg.ReplayWindow = 10 * time.Millisecond
	h := NewHub(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	h.Publish(TopicPoolsNew, "eth", 1)
	time.Sleep(30 * time.Millisecond)
	h.pruneReplay()

	h.replayMu.Lock()
	n := len(h.replay)
	h.replayMu.Unlock()
	if n != 0 {
		t.Errorf("replay buffer kept %d frames past the window", n)
	}
}
