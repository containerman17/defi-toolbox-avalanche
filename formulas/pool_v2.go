package formulas

import (
	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// brokenTokens lists tokens whose transfer() always reverts due to corrupted
// internal state (e.g. broken reflection accounting).
var brokenTokens = map[common.Address]bool{
	common.HexToAddress("0x704eae6d452ca63ce479c59727177c5f3ba0d90c"): true, // EVDC: SafeMath overflow in reflection _transfer
	common.HexToAddress("0xe80772eaf6e2e18b651f160bc9158b2a5cafca65"): true, // USD+: rebasing token, rayDiv rounding causes V2 swap reverts
	common.HexToAddress("0xf7d9281e8e363584973f946201b82ba72c965d27"): true, // gAVAX/yyAVAX: ERC1155-backed ERC20, safeTransferFrom reverts in simulation
	common.HexToAddress("0xb2a85c5ecea99187a977ac34303b80acbddfa208"): true, // ROCO: reflection token, EVM balance override doesn't set _rOwned correctly
	common.HexToAddress("0x90842eb834cfd2a1db0b1512b254a18e4d396215"): true, // GoodBridging (GB): reflection token with 1% fee, _rOwned/_rTotal incompatible with EVM override
	common.HexToAddress("0x440abbf18c54b2782a4917b80a1746d3a2c2cce1"): true, // transfer reverts in EVM simulation
	common.HexToAddress("0xbde79b2a371ea759a85901e4a185d03399c089c1"): true, // transfer reverts in EVM simulation
	common.HexToAddress("0xeccb564c288010ec3f437f2d9a7a836bd30da165"): true, // transfer reverts in EVM simulation
	common.HexToAddress("0x6e7f5c0b9f4432716bdd0a77a3601291b9d9e985"): true, // transfer reverts in EVM simulation
	common.HexToAddress("0x1f1fe1ef06ab30a791d6357fdf0a7361b39b1537"): true, // transfer reverts in EVM simulation
	common.HexToAddress("0xf16f18c155ef8fc46f980b23868d208db59cad9c"): true, // transfer reverts in EVM simulation
	common.HexToAddress("0xba515304d8153c4b162dc79f867e152df9c127eb"): true, // transfer reverts in EVM simulation
	common.HexToAddress("0x136acd46c134e8269052c62a67042d6bdedde3c9"): true, // transfer reverts in EVM simulation
	common.HexToAddress("0x556b959d952085405e7c630bc45a34ace73854eb"): true, // transfer reverts in EVM simulation
	common.HexToAddress("0x542fa0b261503333b90fe60c78f2beed16b7b7fd"): true, // transfer reverts in EVM simulation
	common.HexToAddress("0xa3d68b74bf0528fdd07263c60d6488749044914b"): true, // transfer reverts in EVM simulation
	common.HexToAddress("0x32f0c28be6a6ac5d3b471278b77f6971a3141348"): true, // transfer reverts in EVM simulation
	common.HexToAddress("0x27f6c8289550fce67f6b50bed1f519966afe5287"): true, // transfer reverts in EVM simulation
	common.HexToAddress("0x6c6f910a79639dcc94b4feef59ff507c2e843929"): true, // transfer reverts in EVM simulation
	common.HexToAddress("0x74e422837e88c00c998b63293dfecebb7d6680fc"): true, // transfer reverts in EVM simulation
	common.HexToAddress("0x77146784315ba81904d654466968e3a7c196d1f3"): true, // transfer reverts in EVM simulation
	common.HexToAddress("0xb57b25851fe2311cc3fe511c8f10e868932e0680"): true, // transfer reverts in EVM simulation
	common.HexToAddress("0x0442ac2450dc0b2b56d7320afad0b63b1ff8b0d0"): true, // transfer reverts in EVM simulation
}

// V2Pool is a pre-loaded V2 constant product pool.
// Construction reads slot 8 once. Quote is pure math.
type V2Pool struct {
	addr     common.Address
	reserve0 uint256.Int
	reserve1 uint256.Int
	factor   *uint256.Int // fee factor (9970 for 0.3%, 9950 for 0.5%)
	balance0 uint256.Int  // actual token0 balance of the pool (0 = unknown)
	balance1 uint256.Int  // actual token1 balance of the pool (0 = unknown)
	deadDir0 bool         // true = zeroForOne always reverts on-chain
	deadDir1 bool         // true = !zeroForOne always reverts on-chain
}

// SetDeadDirs marks directions where a broken input token causes reverts.
func (p *V2Pool) SetDeadDirs(token0, token1 common.Address) {
	if brokenTokens[token0] {
		p.deadDir0 = true // selling token0 reverts
	}
	if brokenTokens[token1] {
		p.deadDir1 = true // selling token1 reverts
	}
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

func (p *V2Pool) Quote(amountIn *uint256.Int, tokenIn, tokenOut common.Address) uint256.Int {
	zeroForOne := tokenIn.Cmp(tokenOut) < 0

	if zeroForOne && p.deadDir0 {
		return uint256.Int{}
	}
	if !zeroForOne && p.deadDir1 {
		return uint256.Int{}
	}

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
