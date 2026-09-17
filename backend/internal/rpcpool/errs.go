package rpcpool

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/rpc"
)

// ErrNoEndpoint means every endpoint for a chain is breaker-open, demoted or
// evicted. Callers should back off rather than hammer.
var ErrNoEndpoint = errors.New("rpcpool: no healthy endpoint available")

// Public providers express the same conditions a dozen different ways. These
// classifiers exist so the ingester can react to the condition rather than to a
// particular provider's phrasing.

// IsRateLimited reports a provider throttle, and how long it asked us to wait.
func IsRateLimited(err error) (bool, time.Duration) {
	if err == nil {
		return false, 0
	}
	var httpErr rpc.HTTPError
	if errors.As(err, &httpErr) {
		if httpErr.StatusCode == http.StatusTooManyRequests {
			return true, retryAfter(httpErr)
		}
	}
	var rpcErr rpc.Error
	if errors.As(err, &rpcErr) {
		switch rpcErr.ErrorCode() {
		case -32097, -32029, 429:
			return true, 0
		}
	}
	s := strings.ToLower(err.Error())
	for _, frag := range []string{
		"rate limit", "rate-limit", "ratelimit", "too many requests",
		"429", "exceeded the quota", "request limit", "throttl",
		"capacity exceeded", "daily request count exceeded",
	} {
		if strings.Contains(s, frag) {
			return true, 0
		}
	}
	return false, 0
}

func retryAfter(e rpc.HTTPError) time.Duration {
	// go-ethereum surfaces the body but not headers; look for a seconds hint in
	// the body, which several providers include.
	body := strings.ToLower(string(e.Body))
	idx := strings.Index(body, "retry after")
	if idx < 0 {
		return 0
	}
	rest := body[idx:]
	var digits strings.Builder
	for _, r := range rest {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
			continue
		}
		if digits.Len() > 0 {
			break
		}
	}
	if digits.Len() == 0 {
		return 0
	}
	n, err := strconv.Atoi(digits.String())
	if err != nil || n <= 0 || n > 300 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// IsRangeTooLarge reports that a getLogs query asked for more than the provider
// will serve — by block span, by result count, or by response size. The poller
// bisects rather than failing the tick.
func IsRangeTooLarge(err error) bool {
	if err == nil {
		return false
	}
	var rpcErr rpc.Error
	if errors.As(err, &rpcErr) {
		// -32005 is "limit exceeded" in the JSON-RPC reserved space and is what
		// Infura, Alchemy and several public nodes return for oversized sweeps.
		if rpcErr.ErrorCode() == -32005 {
			return true
		}
	}
	s := strings.ToLower(err.Error())
	for _, frag := range []string{
		"query returned more than", "more than 10000 results",
		"block range", "range too large", "range is too large",
		"exceed maximum block range", "limit exceeded",
		"response size exceeded", "query timeout exceeded",
		"too many logs", "result set too large", "logs matched by query exceeds",
		"request entity too large", "payload too large",
	} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}

// IsCapabilityRefused reports a provider that will not serve this *shape* of
// request on its tier — as opposed to one that is failing, throttling, or asking
// for a smaller range. Retrying the same query on the same endpoint can never
// succeed, so the caller should route it elsewhere and remember not to ask
// again for a while.
//
// Real examples, verbatim from free public endpoints:
//
//	"Please specify an address in your request or, to remove restrictions,
//	 order a dedicated full node here"          (publicnode, address-less getLogs)
//	"Archive requests require a personal token" (publicnode, logs behind the tip)
func IsCapabilityRefused(err error) bool {
	if err == nil {
		return false
	}
	var httpErr rpc.HTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusForbidden {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, frag := range []string{
		"specify an address",
		// drpc's free tier, for log queries more than ~100 blocks behind the
		// tip. The wording is generic but the behaviour, measured, is depth.
		"can't route your request to suitable provider",
		"archive requests require",
		"personal token",
		"dedicated full node",
		"requires a paid", "upgrade your plan", "not available on the free",
		"not supported on this tier", "403 forbidden",
	} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}

// IsMethodUnsupported reports a provider that simply does not offer a method —
// common on free tiers for debug_*, trace_* and eth_call state overrides. The
// caller should disable that capability for the endpoint rather than retry.
func IsMethodUnsupported(err error) bool {
	if err == nil {
		return false
	}
	var rpcErr rpc.Error
	if errors.As(err, &rpcErr) {
		if rpcErr.ErrorCode() == -32601 {
			return true
		}
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "method not found") ||
		strings.Contains(s, "not supported") ||
		strings.Contains(s, "unsupported method") ||
		strings.Contains(s, "does not exist/is not available")
}

// IsRetryable distinguishes transient provider failures from real errors like a
// reverted eth_call, which retrying elsewhere will not fix.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if limited, _ := IsRateLimited(err); limited {
		return true
	}
	if isConnError(err) {
		return true
	}
	var httpErr rpc.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode >= 500 || httpErr.StatusCode == http.StatusTooManyRequests
	}
	s := strings.ToLower(err.Error())
	for _, frag := range []string{
		"timeout", "temporarily unavailable", "service unavailable",
		"bad gateway", "internal error", "try again", "node is syncing",
		"header not found", "missing trie node", "unknown block",
	} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}

// IsExecutionReverted identifies a contract revert, which is a legitimate
// answer (the honeypot probe relies on seeing it) rather than a fault.
func IsExecutionReverted(err error) bool {
	if err == nil {
		return false
	}
	var rpcErr rpc.Error
	if errors.As(err, &rpcErr) && rpcErr.ErrorCode() == 3 {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "execution reverted") || strings.Contains(s, "revert")
}
