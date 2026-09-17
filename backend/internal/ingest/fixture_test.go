package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/store"
)

// A simulated chain and a deliberately hostile provider in front of it.
//
// Every behaviour modelled here is one a free public endpoint actually exhibits:
// throttling under load, refusing a log range it advertised as acceptable,
// answering with a 500 for no reason, and serving a chain that reorganises
// under the reader. The point of the tests is that none of them may cause a
// pool to be missed or counted twice.

const (
	factoryAddr = "0x5C69bEe701ef814a2B6a3EDD4B1652CB9cc5aA6f" // Uniswap v2 factory
	wethAddr    = "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"
)

type fakeBlock struct {
	number uint64
	hash   common.Hash
	parent common.Hash
	ts     uint64
	logs   []types.Log
}

type fakeChain struct {
	mu     sync.Mutex
	blocks []*fakeBlock // index 0 is block 1
	// baseTime is block 0's timestamp; block n is baseTime + 12n. Defaults to
	// late 2023, so every block is far older than the live window.
	baseTime uint64
}

func newFakeChain(n int) *fakeChain {
	fc := &fakeChain{baseTime: 1_700_000_000}
	for i := 1; i <= n; i++ {
		fc.append(nil)
	}
	return fc
}

// append adds a block carrying the given pool addresses as PairCreated logs.
func (f *fakeChain) append(pools []string) *fakeBlock {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.appendLocked(pools, "")
}

func (f *fakeChain) appendLocked(pools []string, salt string) *fakeBlock {
	number := uint64(len(f.blocks) + 1)
	var parent common.Hash
	if len(f.blocks) > 0 {
		parent = f.blocks[len(f.blocks)-1].hash
	}
	b := &fakeBlock{
		number: number,
		parent: parent,
		hash:   crypto.Keccak256Hash([]byte(fmt.Sprintf("block-%d-%s", number, salt))),
		ts:     f.baseTime + number*12,
	}
	for i, pool := range pools {
		b.logs = append(b.logs, pairCreatedLog(number, b.hash, pool, uint(i)))
	}
	f.blocks = append(f.blocks, b)
	return b
}

// reorg replaces every block from `from` onward with a different history.
func (f *fakeChain) reorg(from uint64, replacement map[uint64][]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if from == 0 || int(from) > len(f.blocks) {
		return
	}
	f.blocks = f.blocks[:from-1]
	target := from - 1 + uint64(len(replacement))
	for n := from; n <= target; n++ {
		f.appendLocked(replacement[n], "reorged")
	}
}

func (f *fakeChain) head() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return uint64(len(f.blocks))
}

func (f *fakeChain) block(n uint64) *fakeBlock {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n == 0 || int(n) > len(f.blocks) {
		return nil
	}
	return f.blocks[n-1]
}

func (f *fakeChain) logsIn(from, to uint64) []types.Log {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []types.Log
	for n := from; n <= to && int(n) <= len(f.blocks); n++ {
		b := f.blocks[n-1]
		for _, l := range b.logs {
			l.BlockHash = b.hash
			out = append(out, l)
		}
	}
	return out
}

// pairCreatedLog builds a real PairCreated log, encoded exactly as a node would
// return it, so the production decoder is what the test exercises.
func pairCreatedLog(block uint64, blockHash common.Hash, pool string, index uint) types.Log {
	token0 := common.HexToHash(strings.ToLower(pool)) // stand-in token identity
	token1 := common.BytesToHash(common.HexToAddress(wethAddr).Bytes())

	data := make([]byte, 64)
	copy(data[12:32], common.HexToAddress(pool).Bytes())
	data[63] = 1 // allPairsLength

	return types.Log{
		Address: common.HexToAddress(factoryAddr),
		Topics: []common.Hash{
			chain.TopicPairCreatedV2,
			common.BytesToHash(common.BytesToAddress(token0.Bytes()).Bytes()),
			token1,
		},
		Data:        data,
		BlockNumber: block,
		BlockHash:   blockHash,
		TxHash:      crypto.Keccak256Hash([]byte(fmt.Sprintf("tx-%d-%s", block, pool))),
		Index:       index,
	}
}

// hostile is the provider's misbehaviour profile.
type hostile struct {
	// rateLimitEvery throttles one in every N requests with a 429.
	rateLimitEvery int
	// maxLogRange refuses any getLogs span wider than this, the way a provider
	// silently enforces a limit it never published.
	maxLogRange uint64
	// serverErrorEvery returns a 500 one request in N.
	serverErrorEvery int
	// requireAddress refuses any getLogs query without an address filter, with
	// publicnode's exact wording. Observed live on BSC and Arbitrum.
	requireAddress bool
	// refuseLogs refuses every getLogs query while answering everything else
	// normally — the shape that kept a failing endpoint top-scored, because its
	// eth_blockNumber checks kept succeeding. Also observed live.
	refuseLogs bool
	// archiveDepth refuses log queries starting more than this many blocks
	// behind head, with drpc's wording. Zero disables it. Measured live: drpc's
	// free tier serves ~100 blocks back and refuses beyond.
	archiveDepth uint64
	// refuseLogsEvery refuses one log query in N as "archive", the way
	// publicnode's load-balanced backends do: the same query succeeds on retry.
	refuseLogsEvery int

	mu              sync.Mutex
	requests        int
	rateLimited     int
	rangeRefused    int
	serverErrored   int
	getLogsCalls    int
	addressRefusals int
	logsRefusals    int
	logsSeen        int
}

func (h *hostile) stats() (requests, limited, refused, errored, getLogs int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.requests, h.rateLimited, h.rangeRefused, h.serverErrored, h.getLogsCalls
}

type rpcRequest struct {
	ID     json.RawMessage   `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// newFixtureServer serves JSON-RPC over HTTP against the fake chain.
func newFixtureServer(fc *fakeChain, h *hostile) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// go-ethereum may batch. The response shape has to mirror the request
		// shape exactly, or the client rejects it.
		isBatch := firstNonSpace(raw) == '['
		var batch []rpcRequest
		if isBatch {
			if err := json.Unmarshal(raw, &batch); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
		} else {
			var single rpcRequest
			if err := json.Unmarshal(raw, &single); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			batch = []rpcRequest{single}
		}

		h.mu.Lock()
		h.requests++
		n := h.requests
		throttle := h.rateLimitEvery > 0 && n%h.rateLimitEvery == 0
		fail := h.serverErrorEvery > 0 && n%h.serverErrorEvery == 0
		if throttle {
			h.rateLimited++
		}
		if fail {
			h.serverErrored++
		}
		h.mu.Unlock()

		if throttle {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			// No Retry-After hint, which is how most free tiers answer: the
			// client is left to back off on its own judgement.
			_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
			return
		}
		if fail {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		responses := make([]rpcResponse, 0, len(batch))
		for _, req := range batch {
			responses = append(responses, serve(fc, h, req))
		}

		w.Header().Set("Content-Type", "application/json")
		if isBatch {
			_ = json.NewEncoder(w).Encode(responses)
			return
		}
		_ = json.NewEncoder(w).Encode(responses[0])
	}))
}

func firstNonSpace(b []byte) byte {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return c
		}
	}
	return 0
}

func serve(fc *fakeChain, h *hostile, req rpcRequest) rpcResponse {
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "eth_blockNumber":
		resp.Result = hexutil.EncodeUint64(fc.head())

	case "eth_getBlockByNumber":
		var numHex string
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params[0], &numHex)
		}
		n, err := hexutil.DecodeUint64(numHex)
		if err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid block number"}
			break
		}
		b := fc.block(n)
		if b == nil {
			resp.Result = nil
			break
		}
		resp.Result = map[string]any{
			"number":     hexutil.EncodeUint64(b.number),
			"hash":       b.hash,
			"parentHash": b.parent,
			"timestamp":  hexutil.EncodeUint64(b.ts),
		}

	case "eth_getLogs":
		h.mu.Lock()
		h.getLogsCalls++
		h.mu.Unlock()

		var q struct {
			FromBlock string            `json:"fromBlock"`
			ToBlock   string            `json:"toBlock"`
			Address   json.RawMessage   `json:"address"`
			Topics    []json.RawMessage `json:"topics"`
		}
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params[0], &q)
		}
		from, err1 := hexutil.DecodeUint64(q.FromBlock)
		to, err2 := hexutil.DecodeUint64(q.ToBlock)
		if err1 != nil || err2 != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid range"}
			break
		}

		addrs := decodeAddressFilter(q.Address)
		var topic0 []string
		if len(q.Topics) > 0 {
			topic0 = decodeAddressFilter(q.Topics[0]) // same string-or-array encoding
		}

		h.mu.Lock()
		h.logsSeen++
		flaky := h.refuseLogsEvery > 0 && h.logsSeen%h.refuseLogsEvery == 0
		h.mu.Unlock()
		if h.archiveDepth > 0 && fc.head() > h.archiveDepth && from < fc.head()-h.archiveDepth {
			h.mu.Lock()
			h.logsRefusals++
			h.mu.Unlock()
			resp.Error = &rpcError{Code: 12,
				Message: "Can't route your request to suitable provider, if you specified certain providers revise the list"}
			break
		}
		if flaky {
			h.mu.Lock()
			h.logsRefusals++
			h.mu.Unlock()
			resp.Error = &rpcError{Code: -32602,
				Message: "Archive requests require a personal token. Get one at: https://www.allnodes.com/publicnode"}
			break
		}
		if h.refuseLogs {
			h.mu.Lock()
			h.logsRefusals++
			h.mu.Unlock()
			resp.Error = &rpcError{Code: -32602,
				Message: "Archive requests require a personal token. Get one at: https://www.allnodes.com/publicnode"}
			break
		}
		if h.requireAddress && len(addrs) == 0 {
			h.mu.Lock()
			h.addressRefusals++
			h.mu.Unlock()
			resp.Error = &rpcError{Code: -32701,
				Message: "Please specify an address in your request or, to remove restrictions, order a dedicated full node here: https://www.allnodes.com/bnb/host"}
			break
		}
		if h.maxLogRange > 0 && to-from+1 > h.maxLogRange {
			h.mu.Lock()
			h.rangeRefused++
			h.mu.Unlock()
			// The exact phrasing several public providers use.
			resp.Error = &rpcError{Code: -32005, Message: "query returned more than 10000 results"}
			break
		}

		// Honour the filter the way a real node does. Returning every log
		// regardless would let a query with the wrong address filter pass.
		logs := []types.Log{}
		for _, l := range fc.logsIn(from, to) {
			if len(addrs) > 0 && !containsFold(addrs, l.Address.Hex()) {
				continue
			}
			if len(topic0) > 0 && (len(l.Topics) == 0 || !containsFold(topic0, l.Topics[0].Hex())) {
				continue
			}
			logs = append(logs, l)
		}
		resp.Result = logs

	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

// decodeAddressFilter reads a JSON-RPC filter field that may be null, a single
// string, or an array of strings.
func decodeAddressFilter(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many
	}
	return nil
}

func containsFold(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

// --- in-memory store ---

type memStore struct {
	mu sync.Mutex

	cursor    map[chain.ID]store.Cursor
	hasCursor map[chain.ID]bool
	blocks    map[string]string // chain:number -> hash

	// byLog is the dedupe index the real schema enforces with a unique
	// constraint; the fake must model it or the test proves nothing.
	byLog   map[string]bool
	byAddr  map[string]*chain.Pool
	inserts int

	topicMatches []store.TopicMatch
	watched      []string // topic0 values users are watching
}

func newMemStore() *memStore {
	return &memStore{
		cursor:    map[chain.ID]store.Cursor{},
		hasCursor: map[chain.ID]bool{},
		blocks:    map[string]string{},
		byLog:     map[string]bool{},
		byAddr:    map[string]*chain.Pool{},
	}
}

func (m *memStore) GetCursor(_ context.Context, id chain.ID) (store.Cursor, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cursor[id], m.hasCursor[id], nil
}

func (m *memStore) SetCursor(_ context.Context, id chain.ID, c store.Cursor) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cursor[id] = c
	m.hasCursor[id] = true
	return nil
}

func blockKey(id chain.ID, n uint64) string { return fmt.Sprintf("%s:%d", id, n) }

func (m *memStore) PutBlockHash(_ context.Context, id chain.ID, b store.Block) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blocks[blockKey(id, b.Number)] = b.Hash
	return nil
}

func (m *memStore) GetBlockHash(_ context.Context, id chain.ID, n uint64) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.blocks[blockKey(id, n)]
	return h, ok, nil
}

func (m *memStore) DeleteBlockHashesFrom(_ context.Context, id chain.ID, from uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.blocks {
		var c string
		var n uint64
		if _, err := fmt.Sscanf(k, "%s:%d", &c, &n); err == nil {
			continue
		}
		parts := strings.Split(k, ":")
		if len(parts) != 2 || parts[0] != string(id) {
			continue
		}
		var num uint64
		_, _ = fmt.Sscanf(parts[1], "%d", &num)
		if num >= from {
			delete(m.blocks, k)
		}
	}
	return nil
}

func (m *memStore) PruneBlockHashes(_ context.Context, _ chain.ID, _ uint64) error { return nil }

func (m *memStore) InsertPool(_ context.Context, p chain.Pool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	logKey := fmt.Sprintf("%s:%s:%d", p.ChainID, strings.ToLower(p.CreatedTx), p.LogIndex)
	addrKey := p.Key()
	if m.byLog[logKey] || m.byAddr[addrKey] != nil {
		return false, nil
	}
	m.byLog[logKey] = true
	cp := p
	m.byAddr[addrKey] = &cp
	m.inserts++
	return true, nil
}

func (m *memStore) SetPoolState(_ context.Context, id chain.ID, addr string, st chain.State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := string(id) + ":" + strings.ToLower(addr)
	if p, ok := m.byAddr[key]; ok {
		p.State = st
	}
	return nil
}

func (m *memStore) UnconfirmedBelow(_ context.Context, id chain.ID, block uint64, limit int) ([]chain.Pool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []chain.Pool
	for _, p := range m.byAddr {
		if p.ChainID == id && p.State == chain.StateUnconfirmed && p.CreatedBlock <= block {
			out = append(out, *p)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (m *memStore) ReorgPoolsFrom(_ context.Context, id chain.ID, from uint64) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, p := range m.byAddr {
		if p.ChainID == id && p.CreatedBlock >= from && p.State != chain.StateReorged {
			p.State = chain.StateReorged
			out = append(out, p.Address)
		}
	}
	return out, nil
}

func (m *memStore) RestorePool(_ context.Context, id chain.ID, txHash string, logIndex uint) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.byAddr {
		if p.ChainID != id || p.State != chain.StateReorged {
			continue
		}
		if strings.EqualFold(p.CreatedTx, txHash) && p.LogIndex == logIndex {
			p.State = chain.StateUnconfirmed
			return true, nil
		}
	}
	return false, nil
}

func (m *memStore) InsertTopicMatches(_ context.Context, matches []store.TopicMatch) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.topicMatches = append(m.topicMatches, matches...)
	return nil
}

func (m *memStore) ActiveTopics(_ context.Context, _ chain.ID) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.watched...), nil
}

func (m *memStore) TrackablePools(_ context.Context, id chain.ID, since time.Time) ([]chain.Pool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []chain.Pool
	for _, p := range m.byAddr {
		live := !p.Backfilled && p.RemovedAt == nil && !p.CreatedAt.Before(since)
		if p.ChainID == id && p.State != chain.StateReorged && (live || p.FavoritedAt != nil) {
			out = append(out, *p)
		}
	}
	return out, nil
}

func (m *memStore) UpdateTrackedMarket(context.Context, chain.ID, string, store.Market) error {
	return nil
}

func (m *memStore) ScreenPool(_ context.Context, id chain.ID, addr string, keep bool) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.byAddr[string(id)+":"+strings.ToLower(addr)]
	if !ok {
		return time.Time{}, store.ErrNotFound
	}
	now := time.Now()
	p.ScreenedAt = &now
	p.RemovedAt = nil
	if !keep {
		p.RemovedAt = &now
	}
	return now, nil
}

func (m *memStore) GetPool(_ context.Context, id chain.ID, addr string) (chain.Pool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.byAddr[string(id)+":"+strings.ToLower(addr)]; ok {
		return *p, nil
	}
	return chain.Pool{}, store.ErrNotFound
}

func (m *memStore) GetToken(context.Context, chain.ID, string) (store.TokenRecord, error) {
	return store.TokenRecord{}, store.ErrNotFound
}

func (m *memStore) NativePrice(context.Context, chain.ID) (float64, bool, error) {
	return 0, false, nil
}

func (m *memStore) matchCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.topicMatches)
}

func (m *memStore) livePools() []chain.Pool {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []chain.Pool
	for _, p := range m.byAddr {
		if p.State != chain.StateReorged {
			out = append(out, *p)
		}
	}
	return out
}

func (m *memStore) insertCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inserts
}

// --- recording sink ---

type recordingSink struct {
	mu       sync.Mutex
	detected []chain.Pool
	states   []chain.Pool
	removed  []chain.Pool
	heads    []uint64
}

func (r *recordingSink) PoolDetected(_ context.Context, p chain.Pool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.detected = append(r.detected, p)
}

func (r *recordingSink) PoolStateChanged(_ context.Context, p chain.Pool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, p)
}

func (r *recordingSink) HeadAdvanced(_ context.Context, _ chain.ID, block uint64, _ time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.heads = append(r.heads, block)
}

func (r *recordingSink) TopicMatched(_ context.Context, _ store.TopicMatch) {}

func (r *recordingSink) PoolMarketUpdated(_ context.Context, _ chain.Pool) {}

func (r *recordingSink) PoolRemoved(_ context.Context, p chain.Pool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = append(r.removed, p)
}

func (r *recordingSink) detectedAddresses() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int{}
	for _, p := range r.detected {
		out[strings.ToLower(p.Address)]++
	}
	return out
}
