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
	factor   *uint256.Int // fee factor (9970 for 0.3%, 9950 for 0.5%)
	balance0 uint256.Int  // actual token0 balance of the pool (0 = unknown)
	balance1 uint256.Int  // actual token1 balance of the pool (0 = unknown)
}

// SetTokenBalances reads actual token balances via EVM and stores them.
// If the actual balance is less than the reserve, swaps in that direction
// will revert on-chain (SafeMath: subtraction overflow), so Quote returns 0.
func (p *V2Pool) SetTokenBalances(caller EVMCaller, token0, token1 common.Address) {
	if caller == nil {
		return
	}
	calldata := make([]byte, 36)
	copy(calldata[:4], balanceOfSelector[:])
	copy(calldata[16:36], p.addr[:])

	if ret, ok := caller(token0, calldata); ok && len(ret) >= 32 {
		p.balance0.SetBytes(ret[:32])
	}
	if ret, ok := caller(token1, calldata); ok && len(ret) >= 32 {
		p.balance1.SetBytes(ret[:32])
	}

}

func newV2Pool(addr common.Address, reader StorageReader) *V2Pool {
	return newV2PoolWithSlot(addr, reader, reservesSlot, factor)
}

// newV2PoolWithSlot constructs a V2Pool reading reserves from a custom storage slot
// and using a custom fee factor. This supports non-standard V2 forks like Hurricane
// (reserves at slot 11, 0.5% fee) and Fraxswap (reserves at slot 28).
func newV2PoolWithSlot(addr common.Address, reader StorageReader, slot common.Hash, feeFactor *uint256.Int) *V2Pool {
	raw := reader(addr, slot)
	data := raw.Bytes() // 32 bytes, left-padded

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
		factor:   feeFactor,
	}
}

// newHurricanePool constructs a V2Pool for Hurricane DEX pools.
// Hurricane stores reserves at slot 11 (not 8) due to extra ERC20 storage (name, symbol, decimals).
// Fee is 0.3% or 0.5% depending on crossPair flag at slot 10 (packed with token1).
func newHurricanePool(addr common.Address, reader StorageReader) *V2Pool {
	// Read crossPair from slot 10: token1 (20 bytes, left-padded to 32) + crossPair (1 byte)
	// In Solidity, address occupies the low 20 bytes, bool occupies the next byte up.
	// Layout: [0..11 padding][crossPair 1 byte][token1 20 bytes]
	slot10 := reader(addr, hurricaneFeeSlot)
	data10 := slot10.Bytes()
	crossPair := data10[11] != 0 // byte just above the 20-byte address

	f := hurricaneFactor30
	if crossPair {
		f = hurricaneFactor50
	}
	return newV2PoolWithSlot(addr, reader, hurricaneReservesSlot, f)
}

// newFraxswapPool constructs a V2Pool for Fraxswap TWAMM pools.
// Fraxswap stores reserves at slot 28 (shifted by longTermOrders struct + TWAMM fields).
// The fee factor is read from slot 24 (stored as 10000 - bps, e.g. 9970 for 0.3%).
func newFraxswapPool(addr common.Address, reader StorageReader) *V2Pool {
	// Read fee from slot 24 (uint256, stored as factor directly: 9970 = 0.3%)
	feeRaw := reader(addr, fraxswapFeeSlot)
	var feeVal uint256.Int
	feeVal.SetBytes(feeRaw.Bytes())
	if feeVal.IsZero() || feeVal.Gt(tenK) {
		return nil // invalid fee
	}
	return newV2PoolWithSlot(addr, reader, fraxswapReservesSlot, &feeVal)
}

func (p *V2Pool) Address() common.Address {
	return p.addr
}

func (p *V2Pool) Quote(amountIn *uint256.Int, zeroForOne bool) uint256.Int {
	var reserveIn, reserveOut, balanceOut *uint256.Int
	if zeroForOne {
		reserveIn = &p.reserve0
		reserveOut = &p.reserve1
		balanceOut = &p.balance1
	} else {
		reserveIn = &p.reserve1
		reserveOut = &p.reserve0
		balanceOut = &p.balance0
	}

	// amountOut = (amountIn * factor * reserveOut) / (reserveIn * 10000 + amountIn * factor)
	f := p.factor
	if f == nil {
		f = factor // default 9970 (0.3% fee)
	}
	var num, den, tmp uint256.Int
	num.Mul(amountIn, f)
	num.Mul(&num, reserveOut)

	den.Mul(reserveIn, tenK)
	tmp.Mul(amountIn, f)
	den.Add(&den, &tmp)

	if den.IsZero() {
		return uint256.Int{}
	}

	var result uint256.Int
	result.Div(&num, &den)

	// If we know the actual token balance and the output exceeds it,
	// the on-chain transfer would revert (SafeMath: subtraction overflow).
	if !balanceOut.IsZero() && result.Gt(balanceOut) {
		return uint256.Int{}
	}

	return result
}
