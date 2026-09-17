package enrich

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// explorerVerified asks an Etherscan-compatible API whether a contract's source
// is published. Any ambiguity — a network failure, an unexpected shape, a rate
// limit — returns "not established", never a guess in either direction.
func (e *Enricher) explorerVerified(ctx context.Context, api, key, address string) (bool, bool) {
	endpoint, err := url.Parse(api)
	if err != nil {
		return false, false
	}
	q := endpoint.Query()
	q.Set("module", "contract")
	q.Set("action", "getabi")
	q.Set("address", address)
	q.Set("apikey", key)
	endpoint.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return false, false
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, false
	}

	var body struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Result  string `json:"result"`
	}
	if err := decodeJSON(resp.Body, &body); err != nil {
		return false, false
	}

	switch body.Status {
	case "1":
		return true, true
	case "0":
		// The explorer answered, and the answer is "no ABI published".
		if body.Result == "Contract source code not verified" ||
			body.Message == "NOTOK" && body.Result == "Contract source code not verified" {
			return false, true
		}
		return false, false // some other NOTOK (bad key, rate limit): unknown
	default:
		return false, false
	}
}
