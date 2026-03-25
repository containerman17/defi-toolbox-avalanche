package formulas

import (
	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// V2Pool is a pre-loaded V2 constant product pool.
// Construction reads slot 8 once. Quote is pure math.
type V2Pool struct {
	addr     common.Address
	reserve0 uint256.Int
	reserve1 uint256.Int
}

func newV2Pool(addr common.Address, reader StorageReader) *V2Pool {
	slot8 := reader(addr, reservesSlot)
	data := slot8.Bytes() // 32 bytes, left-padded

	var reserve0, reserve1 uint256.Int
	reserve0.SetBytes(data[18:32])
	reserve1.SetBytes(data[4:18])

	if reserve0.IsZero() || reserve1.IsZero() {
		return nil
	}

	return &V2Pool{
		addr:     addr,
		reserve0: reserve0,
		reserve1: reserve1,
	}
}

func (p *V2Pool) Address() common.Address {
	return p.addr
}

func (p *V2Pool) Quote(amountIn *uint256.Int, zeroForOne bool) (*uint256.Int, bool) {
	var reserveIn, reserveOut *uint256.Int
	if zeroForOne {
		reserveIn = &p.reserve0
		reserveOut = &p.reserve1
	} else {
		reserveIn = &p.reserve1
		reserveOut = &p.reserve0
	}

	// amountOut = (amountIn * 9970 * reserveOut) / (reserveIn * 10000 + amountIn * 9970)
	var num, den, tmp uint256.Int
	num.Mul(amountIn, factor)
	num.Mul(&num, reserveOut)

	den.Mul(reserveIn, tenK)
	tmp.Mul(amountIn, factor)
	den.Add(&den, &tmp)

	if den.IsZero() {
		return nil, false
	}

	var result uint256.Int
	result.Div(&num, &den)

	if result.IsZero() {
		return nil, false
	}

	return &result, true
}
