package config

import (
	"testing"

	"github.com/earlypair/backend/internal/chain"
)

// TestShippedConfigLoads is a guard against the config file and the structs
// drifting apart. A YAML key that no longer binds fails silently — the value
// simply stays at its default — so the assertions below check that the fields
// the system actually depends on came through.
func TestShippedConfigLoads(t *testing.T) {
	cfg, err := Load("../../config/earlypair.yaml")
	if err != nil {
		t.Fatalf("shipped config does not load: %v", err)
	}

	reg := cfg.Registry()
	if len(reg.IDs()) == 0 {
		t.Fatal("no chains enabled in the shipped config")
	}

	ingesting := 0
	for _, ch := range reg.All() {
		if ch.Kind != chain.KindEVM {
			continue
		}
		// A chain with no factories is allowed on purpose (RPC and signing
		// before discovery); it idles and /chains reports ingesting=false.
		if len(ch.Factories) > 0 {
			ingesting++
			if topics := chain.CreationTopics(ch.Factories); len(topics) == 0 {
				t.Errorf("chain %s has factories but no decodable creation events", ch.ID)
			}
		}
		if ch.Multicall3 == "" {
			t.Errorf("chain %s has no multicall3; token metadata would cost one call per field", ch.ID)
		}
		if ch.BlockTime <= 0 {
			t.Errorf("chain %s has no block time; the poll interval would be degenerate", ch.ID)
		}
		if ch.Confirmations == 0 {
			t.Errorf("chain %s has no confirmation depth; pools would never leave unconfirmed", ch.ID)
		}
	}
	if ingesting == 0 {
		t.Error("no enabled EVM chain has factories; the whole feed would be empty")
	}
}

// TestValidateRejectsWebsocketOnlyChain protects the core design rule: polling
// is the source of truth, so a chain that can only be reached over a socket
// cannot be ingested correctly and must fail loudly at startup.
func TestValidateRejectsWebsocketOnlyChain(t *testing.T) {
	cfg := Default()
	cfg.Chains = []chain.Chain{{ID: "x", Kind: chain.KindEVM, Enabled: true}}
	cfg.Endpoints = []Endpoint{
		{ID: "x-ws", ChainID: "x", Transport: chain.WS, URL: "wss://example.invalid"},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("a chain with only a websocket endpoint was accepted")
	}
}

func TestValidateCatchesTransportMismatch(t *testing.T) {
	cfg := Default()
	cfg.Chains = []chain.Chain{{ID: "x", Kind: chain.KindEVM, Enabled: true}}
	cfg.Endpoints = []Endpoint{
		{ID: "x-http", ChainID: "x", Transport: chain.HTTP, URL: "https://ok.invalid"},
		{ID: "x-ws", ChainID: "x", Transport: chain.WS, URL: "https://not-a-socket.invalid"},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("a ws endpoint with an http url was accepted")
	}
}

func TestValidateRejectsDuplicateEndpointIDs(t *testing.T) {
	cfg := Default()
	cfg.Chains = []chain.Chain{{ID: "x", Kind: chain.KindEVM, Enabled: true}}
	cfg.Endpoints = []Endpoint{
		{ID: "dup", ChainID: "x", Transport: chain.HTTP, URL: "https://a.invalid"},
		{ID: "dup", ChainID: "x", Transport: chain.HTTP, URL: "https://b.invalid"},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("duplicate endpoint ids were accepted")
	}
}

// TestMaskURLNeverLeaksASecret is the one that matters for logs: an endpoint
// URL reaches the health panel and every warning line, so a key in it is a key
// on disk and on someone's screen.
func TestMaskURLNeverLeaksASecret(t *testing.T) {
	secrets := []string{
		"https://eth-mainnet.g.alchemy.com/v2/AbCdEf1234567890xyz",
		"https://mainnet.infura.io/v3/0123456789abcdef0123456789abcdef",
		"https://rpc.example.com/?apikey=sk-live-9f8e7d6c5b4a",
		"wss://user:hunter2@ws.example.com/feed",
	}
	leaked := []string{
		"AbCdEf1234567890xyz",
		"0123456789abcdef0123456789abcdef",
		"sk-live-9f8e7d6c5b4a",
		"hunter2",
	}
	for i, raw := range secrets {
		masked := MaskURL(raw)
		if contains(masked, leaked[i]) {
			t.Errorf("MaskURL(%q) leaked %q: %s", raw, leaked[i], masked)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
