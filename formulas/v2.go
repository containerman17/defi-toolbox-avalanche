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

