package formulas

import (
	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// V2/LFJ V1 constant product formula: amountOut = (amountIn * 9970 * reserveOut) / (reserveIn * 10000 + amountIn * 9970)
// Reserves are packed in storage slot 8: reserve0 (bytes 18-32), reserve1 (bytes 4-18).

var (
	reservesSlot = common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000008")
	factor       = uint256.NewInt(9970)
	tenK         = uint256.NewInt(10000)

	// Non-standard V2 fork storage slots and fee factors
	hurricaneReservesSlot = common.HexToHash("0x000000000000000000000000000000000000000000000000000000000000000b") // slot 11
	hurricaneFeeSlot      = common.HexToHash("0x000000000000000000000000000000000000000000000000000000000000000a") // slot 10 (token1 + crossPair)
	hurricaneFactor30     = uint256.NewInt(9970) // 0.3% fee (crossPair=false): factor = 10000 - 30 = 9970
	hurricaneFactor50     = uint256.NewInt(9950) // 0.5% fee (crossPair=true):  factor = 10000 - 50 = 9950
	fraxswapReservesSlot  = common.HexToHash("0x000000000000000000000000000000000000000000000000000000000000001c") // slot 28
	fraxswapFeeSlot       = common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000018") // slot 24
)

// StorageReader reads a storage slot value. Used to decouple from StateDB.
type StorageReader func(addr common.Address, key common.Hash) common.Hash

// QuoteV2 computes the V2/LFJ V1 constant product swap output.
// Returns the ABI-encoded uint256 result and true if successful.
// Returns nil, false if reserves are zero or the formula doesn't apply.
func QuoteV2(readStorage StorageReader, pool common.Address, tokenIn, tokenOut common.Address, amountIn *uint256.Int) ([]byte, bool) {
	// Read slot 8
	slot8 := readStorage(pool, reservesSlot)
	data := slot8.Bytes() // 32 bytes, left-padded

	// Parse reserves: reserve0 = bytes 18..32, reserve1 = bytes 4..18
	var reserve0, reserve1 uint256.Int
	reserve0.SetBytes(data[18:32])
	reserve1.SetBytes(data[4:18])

	if reserve0.IsZero() || reserve1.IsZero() {
		return nil, false
	}

	// Determine direction: token0 < token1 (Ethereum address ordering)
	zeroForOne := tokenIn.Cmp(tokenOut) < 0

	var reserveIn, reserveOut *uint256.Int
	if zeroForOne {
		reserveIn = &reserve0
		reserveOut = &reserve1
	} else {
		reserveIn = &reserve1
		reserveOut = &reserve0
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

	// ABI-encode as uint256 (32 bytes, left-padded)
	var ret [32]byte
	result.WriteToSlice(ret[:])
	return ret[:], true
}
