package rpcpool

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/obs"
)

// The doorbell.
//
// Everything in this file is advisory. A frame arriving on a subscription tells
// the ingester "a new block probably exists, go look" — nothing more. We never
// read the payload's logs and write them to the database, because a public
// socket offers no replay: whatever it dropped while disconnected is simply
// gone, and we would have no way to know. The poller, which holds a persisted
// cursor, is the only thing allowed to decide what happened on chain.
//
// What this buys is latency. A newHeads push typically lands a few hundred
// milliseconds before the next poll tick would have fired. If every socket in
// the fleet dies, detection gets about a second slower and nothing else
// changes — which is the whole point of building it this way.

// SubSpec describes what a chain's sockets should watch.
type SubSpec struct {
	Addresses []common.Address
	Topics    [][]common.Hash
}

// Doorbell carries block-number hints from the sockets to the ingester.
type Doorbell struct {
	C <-chan uint64
}

// StartSubscriptions launches one supervised socket per WebSocket endpoint on
// the chain and returns the shared doorbell. It never blocks: a chain with no
// working socket simply never rings, and the poller's fallback timer carries it.
func (p *Pool) StartSubscriptions(ctx context.Context, ch chain.Chain, spec SubSpec) Doorbell {
	bell := make(chan uint64, 64)

	for _, ep := range p.AllEndpoints(ch.ID) {
		if ep.Transport != chain.WS {
			continue
		}
		go p.superviseSocket(ctx, ch, ep, spec, bell)
	}
	return Doorbell{C: bell}
}

// superviseSocket keeps one endpoint's socket alive forever: dial, subscribe,
// watch for silence, reconnect with jittered backoff, and demote to HTTP-only
// when the endpoint proves it cannot hold a connection.
func (p *Pool) superviseSocket(ctx context.Context, ch chain.Chain, ep *Endpoint, spec SubSpec, bell chan<- uint64) {
	log := p.log.With("component", "ws", "endpoint", ep.ID, "chain", ch.ID, "url", ep.Masked)
	backoff := p.policy.WSBackoffMin
	if backoff <= 0 {
		backoff = 500 * time.Millisecond
	}
	maxBackoff := p.policy.WSBackoffMax
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}

	for {
		if ctx.Err() != nil {
			return
		}

		// Respect an active demotion: the endpoint has shown it drops, so we
		// stop paying for reconnect attempts and let HTTP carry this chain.
		if demoted, until := ep.wsDemoted(time.Now()); demoted {
			wait := time.Until(until)
			log.Info("socket demoted to http-only, holding off", "until", until, "for", wait)
			if !sleepCtx(ctx, wait) {
				return
			}
			continue
		}

		err := p.runSocket(ctx, ch, ep, spec, bell)
		if ctx.Err() != nil {
			return
		}

		obs.WSReconnects.WithLabelValues(ep.ID).Inc()
		demoted, until := ep.noteWSDrop(err)
		if demoted {
			obs.WSDemotions.WithLabelValues(ep.ID).Inc()
			log.Warn("socket dropped repeatedly; falling back to http for this endpoint",
				"error", err, "until", until)
			continue
		}
		log.Warn("socket dropped, will resubscribe", "error", err, "backoff", backoff)

		if !sleepCtx(ctx, jitter(backoff)) {
			return
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runSocket holds one connection until it fails, returning the reason.
func (p *Pool) runSocket(ctx context.Context, ch chain.Chain, ep *Endpoint, spec SubSpec, bell chan<- uint64) error {
	dialCtx, cancelDial := context.WithTimeout(ctx, 15*time.Second)
	client, err := rpc.DialWebsocket(dialCtx, ep.rawURL, "")
	cancelDial()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer client.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	heads := make(chan json.RawMessage, 64)
	headSub, err := client.EthSubscribe(ctx, heads, "newHeads")
	if err != nil {
		return fmt.Errorf("subscribe newHeads: %w", err)
	}
	defer headSub.Unsubscribe()
	subs := 1

	// The logs subscription is optional: some public endpoints allow newHeads
	// but refuse filtered log subscriptions. Losing it only costs us the extra
	// few hundred milliseconds between a log landing and the next head.
	logs := make(chan json.RawMessage, 256)
	var logSub *rpc.ClientSubscription
	if len(spec.Addresses) > 0 || len(spec.Topics) > 0 {
		arg := map[string]any{}
		if len(spec.Addresses) > 0 {
			arg["address"] = spec.Addresses
		}
		if len(spec.Topics) > 0 {
			topics := make([]any, len(spec.Topics))
			for i, set := range spec.Topics {
				switch len(set) {
				case 0:
					topics[i] = nil
				case 1:
					topics[i] = set[0]
				default:
					topics[i] = set
				}
			}
			arg["topics"] = topics
		}
		logSub, err = client.EthSubscribe(ctx, logs, "logs", arg)
		if err != nil {
			p.log.Debug("endpoint refused a filtered logs subscription; heads only",
				"endpoint", ep.ID, "error", err)
		} else {
			subs++
			defer logSub.Unsubscribe()
		}
	}

	ep.noteWSConnected(subs)
	p.log.Info("socket subscribed", "endpoint", ep.ID, "chain", ch.ID,
		"url", ep.Masked, "subscriptions", subs)

	// Liveness: a public socket that stays OPEN while delivering nothing is the
	// dominant failure mode, and it looks perfectly healthy from the outside.
	// Three block times of silence is treated as death.
	silence := 3 * ch.ExpectedBlockTime()
	if silence < 15*time.Second {
		silence = 15 * time.Second
	}
	idle := time.NewTimer(silence)
	defer idle.Stop()

	var logErr <-chan error
	if logSub != nil {
		logErr = logSub.Err()
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err := <-headSub.Err():
			if err == nil {
				err = fmt.Errorf("newHeads subscription closed")
			}
			return err

		case err := <-logErr:
			// Losing only the log subscription is not fatal; keep the socket and
			// carry on with heads, which still ring the doorbell every block.
			p.log.Debug("logs subscription ended, continuing on heads",
				"endpoint", ep.ID, "error", err)
			logErr = nil
			ep.noteWSConnected(1)

		case raw := <-heads:
			if n, ok := blockNumberFromHead(raw); ok {
				ep.noteHead(n)
				p.recomputeFleetHead(ep.ChainID)
				ringBell(bell, n)
			}
			resetTimer(idle, silence)

		case raw := <-logs:
			// Deliberately ignoring the payload. We take the block number and
			// throw the rest away — see the note at the top of this file.
			if n, ok := blockNumberFromLog(raw); ok {
				ringBell(bell, n)
			}
			resetTimer(idle, silence)

		case <-idle.C:
			return fmt.Errorf("socket silent for %s (open but not delivering)", silence)
		}
	}
}

// ringBell delivers a hint without ever blocking the socket reader. A full
// doorbell means the ingester is already busy, and one more "go look" adds
// nothing.
func ringBell(bell chan<- uint64, n uint64) {
	select {
	case bell <- n:
	default:
	}
}

func blockNumberFromHead(raw json.RawMessage) (uint64, bool) {
	var h struct {
		Number *hexutil.Big `json:"number"`
	}
	if err := json.Unmarshal(raw, &h); err != nil || h.Number == nil {
		return 0, false
	}
	return h.Number.ToInt().Uint64(), true
}

func blockNumberFromLog(raw json.RawMessage) (uint64, bool) {
	var l struct {
		BlockNumber *hexutil.Uint64 `json:"blockNumber"`
	}
	if err := json.Unmarshal(raw, &l); err != nil || l.BlockNumber == nil {
		return 0, false
	}
	return uint64(*l.BlockNumber), true
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// jitter spreads reconnects so a provider blip does not turn into a synchronised
// stampede from every chain we run.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(d))) + d/2
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
