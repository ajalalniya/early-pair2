package clmm

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"

	"github.com/earlypair/backend/internal/rpcpool"
)

// V3Pool reads a Uniswap v3 pool, which answers for itself.
type V3Pool struct{ Address common.Address }

func (p V3Pool) target() common.Address { return p.Address }

func (p V3Pool) slot0() []byte     { return pack(poolV3, "slot0") }
func (p V3Pool) liquidity() []byte { return pack(poolV3, "liquidity") }
func (p V3Pool) spacingCall() []byte {
	return pack(poolV3, "tickSpacing")
}
func (p V3Pool) bitmap(word int16) []byte { return pack(poolV3, "tickBitmap", word) }
func (p V3Pool) tick(t int32) []byte      { return pack(poolV3, "ticks", big.NewInt(int64(t))) }

func (p V3Pool) decodeSlot0(b []byte) (*big.Int, int32, error) {
	vals, err := poolV3.Unpack("slot0", b)
	if err != nil || len(vals) < 2 {
		return nil, 0, fmt.Errorf("clmm: decode slot0: %v", err)
	}
	sqrtX96, _ := vals[0].(*big.Int)
	tick, _ := vals[1].(*big.Int)
	if sqrtX96 == nil || tick == nil {
		return nil, 0, errors.New("clmm: slot0 is missing a field")
	}
	return sqrtX96, int32(tick.Int64()), nil
}

func (p V3Pool) decodeLiquidity(b []byte) (*big.Int, error) { return firstBig(poolV3, "liquidity", b) }
func (p V3Pool) decodeBitmap(b []byte) (*big.Int, error)    { return firstBig(poolV3, "tickBitmap", b) }

func (p V3Pool) decodeTickNet(b []byte) (*big.Int, error) {
	vals, err := poolV3.Unpack("ticks", b)
	if err != nil || len(vals) < 2 {
		return nil, fmt.Errorf("clmm: decode tick: %v", err)
	}
	net, _ := vals[1].(*big.Int)
	if net == nil {
		return nil, errors.New("clmm: tick has no net liquidity")
	}
	return net, nil
}

// V4Pool reads a Uniswap v4 pool through StateView: a v4 pool has no contract
// of its own, only an id inside the PoolManager.
type V4Pool struct {
	StateView common.Address
	PoolID    common.Hash
}

func (p V4Pool) target() common.Address { return p.StateView }

func (p V4Pool) slot0() []byte       { return pack(stateView, "getSlot0", p.PoolID) }
func (p V4Pool) liquidity() []byte   { return pack(stateView, "getLiquidity", p.PoolID) }
func (p V4Pool) spacingCall() []byte { return nil } // the key carries it
func (p V4Pool) bitmap(w int16) []byte {
	return pack(stateView, "getTickBitmap", p.PoolID, w)
}
func (p V4Pool) tick(t int32) []byte {
	return pack(stateView, "getTickLiquidity", p.PoolID, big.NewInt(int64(t)))
}

func (p V4Pool) decodeSlot0(b []byte) (*big.Int, int32, error) {
	vals, err := stateView.Unpack("getSlot0", b)
	if err != nil || len(vals) < 2 {
		return nil, 0, fmt.Errorf("clmm: decode v4 slot0: %v", err)
	}
	sqrtX96, _ := vals[0].(*big.Int)
	tick, _ := vals[1].(*big.Int)
	if sqrtX96 == nil || tick == nil {
		return nil, 0, errors.New("clmm: v4 slot0 is missing a field")
	}
	return sqrtX96, int32(tick.Int64()), nil
}

func (p V4Pool) decodeLiquidity(b []byte) (*big.Int, error) {
	return firstBig(stateView, "getLiquidity", b)
}
func (p V4Pool) decodeBitmap(b []byte) (*big.Int, error) {
	return firstBig(stateView, "getTickBitmap", b)
}

func (p V4Pool) decodeTickNet(b []byte) (*big.Int, error) {
	vals, err := stateView.Unpack("getTickLiquidity", b)
	if err != nil || len(vals) < 2 {
		return nil, fmt.Errorf("clmm: decode v4 tick: %v", err)
	}
	net, _ := vals[1].(*big.Int)
	if net == nil {
		return nil, errors.New("clmm: v4 tick has no net liquidity")
	}
	return net, nil
}

// spacingOf asks a v3 pool for its tick spacing; a v4 pool's is part of the
// key the caller already resolved, and is set on the reader.
func spacingOf(ctx context.Context, evm *rpcpool.EVM, multicall common.Address, r Reader) (int32, error) {
	switch p := r.(type) {
	case V4Spacing:
		return p.Spacing(), nil
	case V3Pool:
		res, err := aggregate(ctx, evm, multicall, []call3{{Target: p.Address, AllowFailure: true, CallData: p.spacingCall()}})
		if err != nil {
			return 0, err
		}
		if len(res) == 0 || !res[0].Success {
			return 0, errors.New("clmm: the pool did not answer for its tick spacing")
		}
		v, err := firstBig(poolV3, "tickSpacing", res[0].ReturnData)
		if err != nil {
			return 0, err
		}
		return int32(v.Int64()), nil
	}
	return 0, errors.New("clmm: unknown pool kind")
}

// V4Spacing lets a v4 reader carry the spacing from its PoolKey, which is
// already resolved and verified against the pool id.
type V4Spacing interface {
	Reader
	Spacing() int32
}

// V4PoolWithSpacing is a V4Pool that knows its tick spacing.
type V4PoolWithSpacing struct {
	V4Pool
	TickSpacing int32
}

func (p V4PoolWithSpacing) Spacing() int32 { return p.TickSpacing }

func pack(a interface {
	Pack(string, ...any) ([]byte, error)
}, method string, args ...any) []byte {
	b, err := a.Pack(method, args...)
	if err != nil {
		return nil
	}
	return b
}

func firstBig(a interface {
	Unpack(string, []byte) ([]any, error)
}, method string, b []byte) (*big.Int, error) {
	vals, err := a.Unpack(method, b)
	if err != nil || len(vals) == 0 {
		return nil, fmt.Errorf("clmm: decode %s: %v", method, err)
	}
	v, _ := vals[0].(*big.Int)
	if v == nil {
		return nil, fmt.Errorf("clmm: %s returned no number", method)
	}
	return v, nil
}
