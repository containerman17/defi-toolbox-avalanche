package formulas

import (
	"math/big"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// v4_u256.go — uint256 version of QuoteV4.
// Uses BytesStateReader for zero big.Int overhead in the hot path.

// v4PoolManagerAddr is the pre-parsed 20-byte address for the PoolManager singleton.
var v4PoolManagerAddr [20]byte

func init() {
	addr := common.HexToAddress(V4PoolManagerAddress)
	copy(v4PoolManagerAddr[:], addr[:])
}

// v4ComputeSwapStepU256 — V4's swap step (exact input only).
func v4ComputeSwapStepU256(
	sqrtPriceCurrentX96, sqrtPriceTargetX96, liquidity, absRemaining *uint256.Int,
	feePips uint32,
) (sqrtPriceNextX96, amountIn, amountOut, feeAmount uint256.Int) {
	zeroForOne := !sqrtPriceCurrentX96.Lt(sqrtPriceTargetX96)

	var feeComplement uint256.Int
	feeComplement.SetUint64(uint64(v4MaxSwapFee) - uint64(feePips))

	amountRemainingLessFee := mulDivU256(absRemaining, &feeComplement, u256Mil)

	if zeroForOne {
		amountIn = sGetAmount0DeltaU256(sqrtPriceTargetX96, sqrtPriceCurrentX96, liquidity, true)
	} else {
		amountIn = sGetAmount1DeltaU256(sqrtPriceCurrentX96, sqrtPriceTargetX96, liquidity, true)
	}

	if !amountRemainingLessFee.Lt(&amountIn) {
		sqrtPriceNextX96.Set(sqrtPriceTargetX96)
		if feePips == uint32(v4MaxSwapFee) {
			feeAmount.Set(&amountIn)
		} else {
			var feePipsU uint256.Int
			feePipsU.SetUint64(uint64(feePips))
			feeAmount = mulDivRoundingUpU256(&amountIn, &feePipsU, &feeComplement)
		}
	} else {
		amountIn.Set(&amountRemainingLessFee)
		sqrtPriceNextX96 = v3GetNextPriceFromInputU256(sqrtPriceCurrentX96, liquidity, &amountRemainingLessFee, zeroForOne)
		feeAmount.Sub(absRemaining, &amountIn)
	}

	if zeroForOne {
		amountOut = sGetAmount1DeltaU256(&sqrtPriceNextX96, sqrtPriceCurrentX96, liquidity, false)
	} else {
		amountOut = sGetAmount0DeltaU256(sqrtPriceCurrentX96, &sqrtPriceNextX96, liquidity, false)
	}
	return
}

// ─── V4 storage helpers using [32]byte slots ───

func v4AddOffset32(stateSlot common.Hash, offset int) [32]byte {
	v := new(big.Int).Add(stateSlot.Big(), big.NewInt(int64(offset)))
	return bigIntTo32(v)
}

