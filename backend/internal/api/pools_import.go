package api

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/earlypair/backend/internal/chain"
	"github.com/earlypair/backend/internal/rpcpool"
	"github.com/earlypair/backend/internal/store"
)

// Adding a pool by address.
//
// The sweep only ever sees pools created while it is watching, and it starts at
// the chain head, so a pair from yesterday is not in the database and cannot be
// traded here at all. This endpoint fills that gap: given an address, it finds
// the pool's own creation log on-chain and stores exactly what the sweep would
// have stored, then stars it so the market tracker keeps it live.
//
// Everything recorded comes from the chain. Nothing is assumed from the address
// itself: an address that is not a pool of a DEX this service watches is
// refused rather than stored as a pool nobody can quote.

// importScanQueries bounds the search for a v4 pool's Initialize log. A v4 pool
// has no contract of its own, so its creation block cannot be found by probing
// for code the way a v2 or v3 pool's can; the log has to be looked for. The
// window this covers depends on the chain's log range — days on a chain that
// serves wide ranges, hours on one that does not — and past it the caller is
// asked for the block rather than the search running unbounded.
const importScanQueries = 12

var poolAddressRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

type importPoolRequest struct {
	ChainID chain.ID `json:"chainId"`
	Address string   `json:"address"`
	// Block optionally names the pool's creation block, which makes the lookup
	// one query instead of a search.
	Block uint64 `json:"block,omitempty"`
}

func (s *Server) handleImportPool(w http.ResponseWriter, r *http.Request) {
	var req importPoolRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	address := strings.ToLower(strings.TrimSpace(req.Address))
	if !poolAddressRe.MatchString(address) && !chain.IsV4PoolID(address) {
		writeError(w, http.StatusBadRequest, "bad_address",
			"give a pool address (0x + 40 hex) or a Uniswap v4 pool id (0x + 64 hex)")
		return
	}
	ch, ok := s.reg.Get(req.ChainID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown_chain", "no such chain")
		return
	}
	if ch.Kind != chain.KindEVM || len(ch.Factories) == 0 {
		writeError(w, http.StatusNotImplemented, "chain_not_indexed",
			"this chain has no DEX factories configured, so its pools cannot be added")
		return
	}
	ctx := r.Context()

	// Already listed: star it and hand it back, rather than reporting an error
	// for what the caller wanted anyway.
	if existing, err := s.store.GetPool(ctx, ch.ID, address); err == nil {
		s.starImported(ctx, existing)
		p, err := s.store.GetPool(ctx, ch.ID, address)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"pool": s.poolResponse(p), "added": false})
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		writeStoreError(w, err)
		return
	}

	pool, err := s.findPool(ctx, ch, address, req.Block)
	if err != nil {
		s.log.Debug("pool import failed", "chain", ch.ID, "address", address, "error", err)
		writeError(w, http.StatusUnprocessableEntity, "pool_not_found", err.Error())
		return
	}
	if _, err := s.store.ImportPool(ctx, pool); err != nil {
		s.log.Error("pool import insert failed", "chain", ch.ID, "address", address, "error", err)
		writeStoreError(w, err)
		return
	}
	s.starImported(ctx, pool)

	saved, err := s.store.GetPool(ctx, ch.ID, address)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.log.Info("pool added by address", "chain", ch.ID, "pool", address, "dex", saved.Dex)
	writeJSON(w, http.StatusOK, map[string]any{"pool": s.poolResponse(saved), "added": true})
}

// starImported stars an added pool. Favorites are what the tracker keeps live,
// and an added pool with frozen columns would be a row of dashes; it is also
// where the UI lists it, so the caller can find what they just added.
func (s *Server) starImported(ctx context.Context, p chain.Pool) {
	if p.FavoritedAt != nil {
		return
	}
	if err := s.store.AddFavorite(ctx, p.ChainID, p.Address); err != nil {
		// A full favorites list is not a reason to refuse the pool: it is stored
		// and tradable either way, it just will not be tracked.
		s.log.Warn("could not star an added pool", "chain", p.ChainID, "pool", p.Address, "error", err)
		return
	}
	if saved, err := s.store.GetPool(ctx, p.ChainID, p.Address); err == nil {
		s.publishFavorite(saved.ChainID, saved.Address, saved.FavoritedAt)
	}
}

// findPool resolves a pool from its address by finding its creation log.
func (s *Server) findPool(ctx context.Context, ch chain.Chain, address string, block uint64) (chain.Pool, error) {
	idx := chain.NewFactoryIndex(ch.Factories)
	topics := chain.CreationTopics(ch.Factories)
	if len(topics) == 0 {
		return chain.Pool{}, errors.New("this chain's factories emit no creation event this service can decode")
	}
	evm := s.pool.EVM(ch.ID)

	// A pool with a contract of its own answers for itself, which is both
	// cheaper and more reliable than hunting for its creation log: public
	// endpoints prune the state that a search needs, and the contract is the
	// authority on its own tokens anyway.
	if block == 0 && !chain.IsV4PoolID(address) {
		return s.poolFromContract(ctx, ch, address)
	}

	var l *types.Log
	var err error
	if block > 0 {
		l, err = s.creationInBlock(ctx, ch, idx, topics, address, block)
	} else {
		l, err = s.findV4Initialize(ctx, ch, address)
	}
	if err != nil {
		return chain.Pool{}, err
	}

	c, err := chain.DecodeCreation(*l, idx)
	if err != nil {
		return chain.Pool{}, fmt.Errorf("that pool's creation event could not be decoded: %w", err)
	}
	if !strings.EqualFold(c.PoolAddress, address) {
		return chain.Pool{}, errors.New("that block's creation event is for a different pool")
	}

	createdAt := time.Now()
	if h, err := evm.HeaderByNumber(ctx, l.BlockNumber); err == nil && h.Timestamp > 0 {
		createdAt = time.Unix(int64(h.Timestamp), 0).UTC()
	}
	p := chain.Pool{
		ChainID: ch.ID, Address: c.PoolAddress, Dex: c.Dex, Model: c.Model,
		// Confirmed on arrival: this pool was read from a log that is already
		// buried, not from the tip where a reorg could still take it away.
		State:        chain.StateConfirmed,
		CreatedAt:    createdAt,
		CreatedBlock: l.BlockNumber,
		CreatedTx:    l.TxHash.Hex(),
		LogIndex:     l.Index,
		FeeTierBps:   c.FeeBps,
		Hooks:        c.Hooks,
		Token0:       &chain.Token{Address: c.Token0},
		Token1:       &chain.Token{Address: c.Token1},
	}
	p.Pair = chain.Orient(ch, p.Token0, p.Token1).Name()
	if err := p.Validate(); err != nil {
		return chain.Pool{}, err
	}
	return p, nil
}

// creationInBlock finds the pool's creation log in one block.
func (s *Server) creationInBlock(ctx context.Context, ch chain.Chain, idx chain.FactoryIndex,
	topics []common.Hash, address string, block uint64) (*types.Log, error) {
	logs, err := s.pool.EVM(ch.ID).FilterLogsHedged(ctx, rpcpool.FilterQuery{
		FromBlock: block, ToBlock: block,
		Addresses: idx.Addresses(),
		Topics:    [][]common.Hash{topics},
	})
	if err != nil {
		return nil, fmt.Errorf("that block's logs could not be read: %w", err)
	}
	for i := range logs {
		if c, err := chain.DecodeCreation(logs[i], idx); err == nil && strings.EqualFold(c.PoolAddress, address) {
			return &logs[i], nil
		}
	}
	return nil, errors.New("no pool creation for that address was found; check the address and the chain, " +
		"or pass the pool's creation block")
}

// findV4Initialize searches recent blocks for a v4 pool's Initialize log,
// filtered by its PoolId, newest first.
func (s *Server) findV4Initialize(ctx context.Context, ch chain.Chain, poolID string) (*types.Log, error) {
	var manager common.Address
	for _, f := range ch.Factories {
		if f.Event == chain.EventInitializeV4 {
			manager = common.HexToAddress(f.Address)
		}
	}
	if manager == (common.Address{}) {
		return nil, errors.New("this chain has no Uniswap v4 PoolManager configured")
	}
	head := s.pool.FleetHead(ch.ID)
	if head == 0 {
		return nil, errors.New("this chain's head is not known yet; try again in a moment")
	}
	span := ch.MaxLogRange
	if span == 0 {
		span = 500
	}
	evm := s.pool.EVM(ch.ID)
	to := head
	for range importScanQueries {
		from := uint64(0)
		if to > span {
			from = to - span + 1
		}
		logs, err := evm.FilterLogsHedged(ctx, rpcpool.FilterQuery{
			FromBlock: from, ToBlock: to,
			Addresses: []common.Address{manager},
			Topics:    [][]common.Hash{{chain.TopicInitializeV4}, {common.HexToHash(poolID)}},
		})
		if err != nil {
			return nil, fmt.Errorf("the chain's logs could not be searched: %w", err)
		}
		if len(logs) > 0 {
			return &logs[0], nil
		}
		if from == 0 {
			break
		}
		to = from - 1
	}
	return nil, fmt.Errorf("that pool was not created in the last %d blocks; pass its creation block to add it",
		span*importScanQueries)
}

// poolFromContract identifies a v2 or v3 pool by asking the pool itself, then
// asking its factory to confirm it. The factory's own lookup is what makes this
// safe: any contract can claim a factory and a pair of tokens, but only the
// real factory maps those tokens and fee back to this address.
func (s *Server) poolFromContract(ctx context.Context, ch chain.Chain, address string) (chain.Pool, error) {
	evm := s.pool.EVM(ch.ID)
	addr := common.HexToAddress(address)

	token0, err := callAddress(ctx, evm, addr, "token0")
	if err != nil {
		return chain.Pool{}, errors.New("that address is not a pool contract this service can read (no token0)")
	}
	token1, err := callAddress(ctx, evm, addr, "token1")
	if err != nil {
		return chain.Pool{}, errors.New("that address is not a pool contract this service can read (no token1)")
	}
	factory, err := callAddress(ctx, evm, addr, "factory")
	if err != nil {
		return chain.Pool{}, errors.New("that pool does not name a factory, so the DEX behind it cannot be established")
	}

	var f chain.Factory
	for _, cand := range ch.Factories {
		if common.HexToAddress(cand.Address) == factory {
			f = cand
		}
	}
	if f.Address == "" {
		return chain.Pool{}, fmt.Errorf("that pool belongs to factory %s, which is not a DEX this service watches on %s",
			factory.Hex(), ch.Name)
	}

	p := chain.Pool{
		ChainID: ch.ID, Address: strings.ToLower(address), Dex: f.Dex, Model: f.Model,
		State: chain.StateConfirmed,
		// The creation log is not readable here — see the comment above — so the
		// pool carries no creating transaction and no launch time. Age reads as
		// unknown rather than as the moment it was added, and the tracker starts
		// its figures from now.
		CreatedAt: time.Now().UTC(),
		Token0:    &chain.Token{Address: strings.ToLower(token0.Hex())},
		Token1:    &chain.Token{Address: strings.ToLower(token1.Hex())},
	}

	switch f.Event {
	case chain.EventPoolCreatedV3:
		fee, err := callUint(ctx, evm, addr, "fee")
		if err != nil {
			return chain.Pool{}, errors.New("that pool's fee tier could not be read")
		}
		bps := int(fee.Int64() / 100)
		p.FeeTierBps = &bps
		got, err := callAddress3(ctx, evm, factory, "getPool", token0, token1, fee)
		if err != nil || got != addr {
			return chain.Pool{}, errors.New("that factory does not list this pool for its own tokens and fee")
		}
	case chain.EventPairCreatedV2, chain.EventPoolCreatedSolidly:
		if f.FeeBps > 0 {
			bps := f.FeeBps
			p.FeeTierBps = &bps
		}
		got, err := callAddress2(ctx, evm, factory, "getPair", token0, token1)
		if err != nil || got != addr {
			return chain.Pool{}, errors.New("that factory does not list this pool for its own tokens")
		}
	default:
		return chain.Pool{}, fmt.Errorf("pools of %s cannot be added by address yet", f.Dex)
	}

	p.Pair = chain.Orient(ch, p.Token0, p.Token1).Name()
	p.ImportedAt = &p.CreatedAt
	if err := p.Validate(); err != nil {
		return chain.Pool{}, err
	}
	return p, nil
}

// poolMini is the slice of the v2/v3 pool and factory interfaces used to
// identify a pool from its address.
var poolMini = mustABI(`[
  {"name":"token0","inputs":[],"outputs":[{"type":"address"}],"stateMutability":"view","type":"function"},
  {"name":"token1","inputs":[],"outputs":[{"type":"address"}],"stateMutability":"view","type":"function"},
  {"name":"factory","inputs":[],"outputs":[{"type":"address"}],"stateMutability":"view","type":"function"},
  {"name":"fee","inputs":[],"outputs":[{"type":"uint24"}],"stateMutability":"view","type":"function"},
  {"name":"getPair","inputs":[{"type":"address"},{"type":"address"}],"outputs":[{"type":"address"}],"stateMutability":"view","type":"function"},
  {"name":"getPool","inputs":[{"type":"address"},{"type":"address"},{"type":"uint24"}],"outputs":[{"type":"address"}],"stateMutability":"view","type":"function"}
]`)

func callAddress(ctx context.Context, evm *rpcpool.EVM, to common.Address, method string, args ...any) (common.Address, error) {
	data, err := poolMini.Pack(method, args...)
	if err != nil {
		return common.Address{}, err
	}
	out, err := evm.CallContract(ctx, to, data, nil, nil)
	if err != nil {
		return common.Address{}, err
	}
	vals, err := poolMini.Unpack(method, out)
	if err != nil || len(vals) == 0 {
		return common.Address{}, fmt.Errorf("api: %s did not return an address", method)
	}
	a, _ := vals[0].(common.Address)
	if a == (common.Address{}) {
		return common.Address{}, fmt.Errorf("api: %s returned the zero address", method)
	}
	return a, nil
}

func callAddress2(ctx context.Context, evm *rpcpool.EVM, to common.Address, method string, a, b common.Address) (common.Address, error) {
	return callAddress(ctx, evm, to, method, a, b)
}

func callAddress3(ctx context.Context, evm *rpcpool.EVM, to common.Address, method string, a, b common.Address, fee *big.Int) (common.Address, error) {
	return callAddress(ctx, evm, to, method, a, b, fee)
}

func callUint(ctx context.Context, evm *rpcpool.EVM, to common.Address, method string) (*big.Int, error) {
	data, err := poolMini.Pack(method)
	if err != nil {
		return nil, err
	}
	out, err := evm.CallContract(ctx, to, data, nil, nil)
	if err != nil {
		return nil, err
	}
	vals, err := poolMini.Unpack(method, out)
	if err != nil || len(vals) == 0 {
		return nil, fmt.Errorf("api: %s did not return a number", method)
	}
	v, _ := vals[0].(*big.Int)
	if v == nil {
		return nil, fmt.Errorf("api: %s returned no number", method)
	}
	return v, nil
}
