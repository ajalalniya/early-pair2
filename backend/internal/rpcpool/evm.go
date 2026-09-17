package rpcpool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/earlypair/backend/internal/chain"
)

// hexUint64 decodes the quantity encoding every EVM RPC uses for block numbers.
type hexUint64 uint64

func (h *hexUint64) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		// Some providers return a plain number despite the spec.
		var n uint64
		if err2 := json.Unmarshal(b, &n); err2 == nil {
			*h = hexUint64(n)
			return nil
		}
		return err
	}
	v, err := hexutil.DecodeUint64(s)
	if err != nil {
		return err
	}
	*h = hexUint64(v)
	return nil
}

// Header is the subset of an EVM block header the ingester needs: enough to
// detect a reorg (hash/parent) and to measure detection latency (timestamp).
type Header struct {
	Number     uint64
	Hash       common.Hash
	ParentHash common.Hash
	Timestamp  uint64
}

type rawHeader struct {
	Number     *hexutil.Big   `json:"number"`
	Hash       common.Hash    `json:"hash"`
	ParentHash common.Hash    `json:"parentHash"`
	Timestamp  hexutil.Uint64 `json:"timestamp"`
}

// EVM is the typed façade business logic uses. Every method routes through the
// pool, so there is exactly one place where an RPC call can be made and
// therefore exactly one place where it can be budgeted, hedged and measured.
type EVM struct {
	pool  *Pool
	chain chain.ID
}

func (p *Pool) EVM(chainID chain.ID) *EVM { return &EVM{pool: p, chain: chainID} }

// BlockNumber returns the chain's head as seen by the fastest healthy endpoint,
// and records it for the quorum view.
func (e *EVM) BlockNumber(ctx context.Context) (uint64, error) {
	var n hexUint64
	if err := e.pool.CallHedged(ctx, e.chain, ClassCheap, &n, "eth_blockNumber"); err != nil {
		return 0, err
	}
	// Record what we just saw. A hedged call does not report which endpoint
	// answered, so this feeds the chain-level head directly; per-endpoint heads
	// and their lag still come from the pool's own sweep. Without this the
	// quorum head stays zero between sweeps, and anything that reads it —
	// confirmations, cursor lag — silently does nothing.
	e.pool.observeHead(e.chain, uint64(n))
	return uint64(n), nil
}

// HeaderByNumber fetches a header without its transactions.
func (e *EVM) HeaderByNumber(ctx context.Context, number uint64) (Header, error) {
	var raw *rawHeader
	err := e.pool.CallHedged(ctx, e.chain, ClassCheap, &raw,
		"eth_getBlockByNumber", hexutil.EncodeUint64(number), false)
	if err != nil {
		return Header{}, err
	}
	if raw == nil || raw.Number == nil {
		return Header{}, fmt.Errorf("rpcpool: block %d not found", number)
	}
	return Header{
		Number:     raw.Number.ToInt().Uint64(),
		Hash:       raw.Hash,
		ParentHash: raw.ParentHash,
		Timestamp:  uint64(raw.Timestamp),
	}, nil
}

// FilterQuery is a getLogs request. Kept local rather than reusing go-ethereum's
// so the JSON encoding stays explicit — providers differ on what they accept.
type FilterQuery struct {
	FromBlock uint64
	ToBlock   uint64
	Addresses []common.Address
	// Topics is the standard positional filter: Topics[0] is the set of
	// acceptable event signatures.
	Topics [][]common.Hash
}

func (q FilterQuery) arg() map[string]any {
	m := map[string]any{
		"fromBlock": hexutil.EncodeUint64(q.FromBlock),
		"toBlock":   hexutil.EncodeUint64(q.ToBlock),
	}
	if len(q.Addresses) > 0 {
		m["address"] = q.Addresses
	}
	if len(q.Topics) > 0 {
		topics := make([]any, len(q.Topics))
		for i, set := range q.Topics {
			switch len(set) {
			case 0:
				topics[i] = nil
			case 1:
				topics[i] = set[0]
			default:
				topics[i] = set
			}
		}
		m["topics"] = topics
	}
	return m
}

// FilterLogs runs eth_getLogs against a single endpoint. The caller owns
// chunking and bisection because it holds the per-endpoint range budget.
func (e *EVM) FilterLogs(ctx context.Context, ep *Endpoint, q FilterQuery) ([]types.Log, error) {
	var logs []types.Log
	err := e.pool.CallOn(ctx, ep, ClassExpensive, &logs, "eth_getLogs", q.arg())
	if err != nil {
		return nil, err
	}
	return logs, nil
}

// FilterLogsHedged runs eth_getLogs across the two fastest endpoints. Used for
// small, latency-sensitive sweeps at the head; wide historical sweeps go
// through FilterLogs on one endpoint so a backfill cannot multiply its cost.
func (e *EVM) FilterLogsHedged(ctx context.Context, q FilterQuery) ([]types.Log, error) {
	var logs []types.Log
	err := e.pool.CallHedged(ctx, e.chain, ClassExpensive, &logs, "eth_getLogs", q.arg())
	if err != nil {
		return nil, err
	}
	return logs, nil
}

// CallContract performs eth_call at a block tag, optionally with state
// overrides. Overrides are how the honeypot probe funds a synthetic buyer
// without touching a real balance; providers that reject the third parameter
// are detected via IsMethodUnsupported by the caller.
func (e *EVM) CallContract(ctx context.Context, to common.Address, data []byte, from *common.Address, overrides map[string]any) ([]byte, error) {
	call := map[string]any{
		"to":   to,
		"data": hexutil.Encode(data),
	}
	if from != nil {
		call["from"] = *from
	}
	args := []any{call, "latest"}
	if len(overrides) > 0 {
		args = append(args, overrides)
	}

	var out hexutil.Bytes
	err := e.pool.CallHedged(ctx, e.chain, ClassNormal, &out, "eth_call", args...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Simulate runs eth_call as from, carrying a native value, optionally with state
// overrides. A payable call — a swap paid in native currency — reverts in a
// simulation that omits its value.
func (e *EVM) Simulate(ctx context.Context, from, to common.Address, data []byte, value *big.Int, overrides map[string]any) ([]byte, error) {
	call := map[string]any{
		"from": from,
		"to":   to,
		"data": hexutil.Encode(data),
	}
	if value != nil && value.Sign() > 0 {
		call["value"] = (*hexutil.Big)(value)
	}
	args := []any{call, "latest"}
	if len(overrides) > 0 {
		args = append(args, overrides)
	}
	var out hexutil.Bytes
	if err := e.pool.CallHedged(ctx, e.chain, ClassNormal, &out, "eth_call", args...); err != nil {
		return nil, err
	}
	return out, nil
}

// CodeAt reports whether an address holds bytecode, used to tell a token
// contract from an EOA before wasting calls on it.
func (e *EVM) CodeAt(ctx context.Context, addr common.Address) ([]byte, error) {
	var out hexutil.Bytes
	err := e.pool.Call(ctx, e.chain, ClassCheap, &out, "eth_getCode", addr, "latest")
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CodeAtBlock reports an address's bytecode as of one block. Finding when a
// contract was created — by searching for the first block it had code — is the
// only use, and it needs an endpoint that keeps historical state.
func (e *EVM) CodeAtBlock(ctx context.Context, addr common.Address, block uint64) ([]byte, error) {
	var out hexutil.Bytes
	err := e.pool.Call(ctx, e.chain, ClassCheap, &out, "eth_getCode", addr, hexutil.EncodeUint64(block))
	if err != nil {
		return nil, err
	}
	return out, nil
}

// StorageAt reads a raw storage slot — the cheapest way to inspect a proxy's
// implementation pointer or an owner slot when there is no accessor.
func (e *EVM) StorageAt(ctx context.Context, addr common.Address, slot common.Hash) ([]byte, error) {
	var out hexutil.Bytes
	err := e.pool.Call(ctx, e.chain, ClassCheap, &out, "eth_getStorageAt", addr, slot, "latest")
	if err != nil {
		return nil, err
	}
	return out, nil
}

// EstimateGas is used only to price a quote; a failure here is reported to the
// user rather than papered over with a guess.
func (e *EVM) EstimateGas(ctx context.Context, from common.Address, to common.Address, data []byte, value *big.Int) (uint64, error) {
	call := map[string]any{
		"from": from,
		"to":   to,
		"data": hexutil.Encode(data),
	}
	if value != nil && value.Sign() > 0 {
		call["value"] = (*hexutil.Big)(value)
	}
	var out hexUint64
	if err := e.pool.Call(ctx, e.chain, ClassNormal, &out, "eth_estimateGas", call); err != nil {
		return 0, err
	}
	return uint64(out), nil
}

// GasPrice returns the current gas price for quote pricing.
func (e *EVM) GasPrice(ctx context.Context) (*big.Int, error) {
	var out hexutil.Big
	if err := e.pool.Call(ctx, e.chain, ClassCheap, &out, "eth_gasPrice"); err != nil {
		return nil, err
	}
	return (*big.Int)(&out), nil
}

// TransactionSender returns the account that signed a transaction.
//
// Uniswap v4 swap events name the contract that called the PoolManager —
// almost always a router — so the signer is the only way to count distinct
// traders. It is a cheap call, but one per transaction, so callers budget it.
func (e *EVM) TransactionSender(ctx context.Context, hash common.Hash) (common.Address, error) {
	var tx *struct {
		From common.Address `json:"from"`
	}
	if err := e.pool.CallHedged(ctx, e.chain, ClassCheap, &tx, "eth_getTransactionByHash", hash); err != nil {
		return common.Address{}, err
	}
	if tx == nil {
		return common.Address{}, fmt.Errorf("rpcpool: transaction %s not found", hash.Hex())
	}
	return tx.From, nil
}

// MinedTx is a mined transaction and its receipt: what a trade record is
// verified against.
type MinedTx struct {
	From        common.Address
	To          *common.Address
	Value       *big.Int
	BlockNumber uint64
	Success     bool
	Logs        []types.Log
}

// ErrNotMined is returned for a transaction the chain does not have a receipt
// for yet — pending, dropped, or not yet seen by the endpoint asked.
var ErrNotMined = errors.New("rpcpool: transaction not mined")

// MinedTransaction reads a transaction and its receipt.
func (e *EVM) MinedTransaction(ctx context.Context, hash common.Hash) (MinedTx, error) {
	var receipt *struct {
		From        common.Address  `json:"from"`
		To          *common.Address `json:"to"`
		Status      hexutil.Uint64  `json:"status"`
		BlockNumber hexutil.Uint64  `json:"blockNumber"`
		Logs        []types.Log     `json:"logs"`
	}
	if err := e.pool.CallHedged(ctx, e.chain, ClassCheap, &receipt, "eth_getTransactionReceipt", hash); err != nil {
		return MinedTx{}, err
	}
	if receipt == nil {
		return MinedTx{}, ErrNotMined
	}
	var tx *struct {
		Value hexutil.Big `json:"value"`
	}
	if err := e.pool.CallHedged(ctx, e.chain, ClassCheap, &tx, "eth_getTransactionByHash", hash); err != nil {
		return MinedTx{}, err
	}
	if tx == nil {
		return MinedTx{}, ErrNotMined
	}
	return MinedTx{
		From: receipt.From, To: receipt.To, Value: (*big.Int)(&tx.Value),
		BlockNumber: uint64(receipt.BlockNumber), Success: receipt.Status == 1, Logs: receipt.Logs,
	}, nil
}

// TransactionSenders resolves many transaction signers at once, using JSON-RPC
// batches so a busy tick costs a handful of requests instead of one per trade.
// It returns whatever it could resolve; missing hashes are simply absent, and
// the caller retries them on its next pass.
func (e *EVM) TransactionSenders(ctx context.Context, hashes []common.Hash) map[common.Hash]common.Address {
	out := make(map[common.Hash]common.Address, len(hashes))
	if len(hashes) == 0 {
		return out
	}
	const batchSize = 25

	for start := 0; start < len(hashes); start += batchSize {
		end := start + batchSize
		if end > len(hashes) {
			end = len(hashes)
		}
		chunk := hashes[start:end]

		for _, ep := range e.pool.Endpoints(e.chain, chain.HTTP) {
			results := make([]*struct {
				From common.Address `json:"from"`
			}, len(chunk))
			elems := make([]rpc.BatchElem, len(chunk))
			for i, h := range chunk {
				elems[i] = rpc.BatchElem{Method: "eth_getTransactionByHash", Args: []any{h}, Result: &results[i]}
			}
			if err := e.pool.BatchOn(ctx, ep, ClassCheap, elems); err != nil {
				if ctx.Err() != nil {
					return out
				}
				continue // try the next endpoint for this chunk
			}
			for i, el := range elems {
				if el.Error == nil && results[i] != nil {
					out[chunk[i]] = results[i].From
				}
			}
			break
		}
	}
	return out
}

// SendRawTransaction relays a transaction the *client* signed. The service
// never signs anything; this exists only so a user behind a restrictive network
// can broadcast through us.
func (e *EVM) SendRawTransaction(ctx context.Context, raw []byte) (common.Hash, error) {
	var h common.Hash
	if err := e.pool.Call(ctx, e.chain, ClassNormal, &h, "eth_sendRawTransaction", hexutil.Encode(raw)); err != nil {
		return common.Hash{}, err
	}
	return h, nil
}

// DecodeRevert extracts the reason string from a reverted eth_call so the user
// sees "TRANSFER_FROM_FAILED" rather than an opaque failure.
func DecodeRevert(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	idx := strings.Index(msg, "execution reverted")
	if idx < 0 {
		return ""
	}
	rest := strings.TrimPrefix(msg[idx:], "execution reverted")
	rest = strings.TrimPrefix(strings.TrimSpace(rest), ":")
	return strings.TrimSpace(rest)
}

// box lets hedged calls decode into private storage so a losing racer cannot
// write into the caller's result after a winner has already returned.
type box struct{ v reflect.Value }

func newBox(out any) *box {
	if out == nil {
		return &box{}
	}
	t := reflect.TypeOf(out)
	if t.Kind() != reflect.Pointer {
		return &box{}
	}
	return &box{v: reflect.New(t.Elem())}
}

func (b *box) ptr() any {
	if !b.v.IsValid() {
		return nil
	}
	return b.v.Interface()
}

func (b *box) value() any {
	if !b.v.IsValid() {
		return nil
	}
	return b.v.Elem().Interface()
}

func assign(out any, val any) {
	if out == nil || val == nil {
		return
	}
	rv := reflect.ValueOf(out)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return
	}
	vv := reflect.ValueOf(val)
	if !vv.Type().AssignableTo(rv.Elem().Type()) {
		return
	}
	rv.Elem().Set(vv)
}
