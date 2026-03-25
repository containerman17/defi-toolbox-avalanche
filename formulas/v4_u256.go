package formulas

import (
	"fmt"
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

// QuoteV4U256 computes the output amount for a V4 swap using uint256 math.
func QuoteV4U256(readBytes BytesStateReader, state *V4State, amountIn *uint256.Int, zeroForOne bool) (uint256.Int, error) {
	if amountIn.IsZero() {
		return uint256.Int{}, nil
	}

	// Determine swap fee
	var protocolFee uint32
	if zeroForOne {
		protocolFee = state.ProtocolFee & 0xFFF
	} else {
		protocolFee = (state.ProtocolFee >> 12) & 0xFFF
	}

	var swapFee uint32
	if protocolFee == 0 {
		swapFee = state.LpFee
	} else {
		swapFee = protocolFee + state.LpFee - (protocolFee*state.LpFee)/uint32(v4MaxSwapFee)
	}

	// Price limit
	var sqrtPriceLimitX96 uint256.Int
	if zeroForOne {
		sqrtPriceLimitX96.Add(u256MinSqr, uint256.NewInt(1))
	} else {
		sqrtPriceLimitX96.Sub(u256MaxSqr, uint256.NewInt(1))
	}

	// Swap state
	var sqrtPriceX96, liquidity uint256.Int
	sqrtPriceX96.SetFromBig(state.SqrtPriceX96)
	liquidity.SetFromBig(state.Liquidity)
	tick := state.Tick

	var absRemaining uint256.Int
	absRemaining.Set(amountIn)

	var amountCalculated uint256.Int
	stateSlot := v4GetPoolStateSlot(state.PoolId)

	// Pre-compute keccak bases as [32]byte
	bitmapSlot := v4AddOffset32(stateSlot, v4TickBitmapOffset)
	ticksSlot := v4AddOffset32(stateSlot, v4TicksOffset)

	for !absRemaining.IsZero() && !sqrtPriceX96.Eq(&sqrtPriceLimitX96) {
		tickNext, initialized, err := v4NextInitTickBytes(
			readBytes, bitmapSlot, tick, state.TickSpacing, zeroForOne,
		)
		if err != nil {
			return uint256.Int{}, fmt.Errorf("nextInitializedTick: %w", err)
		}

		if tickNext <= algebraMinTick {
			tickNext = algebraMinTick
		}
		if tickNext >= algebraMaxTick {
			tickNext = algebraMaxTick
		}

		sqrtPriceNextX96 := getSqrtRatioAtTickU256(tickNext)

		var sqrtPriceTargetX96 *uint256.Int
		if zeroForOne {
			if sqrtPriceNextX96.Lt(&sqrtPriceLimitX96) {
				sqrtPriceTargetX96 = &sqrtPriceLimitX96
			} else {
				sqrtPriceTargetX96 = &sqrtPriceNextX96
			}
		} else {
			if sqrtPriceLimitX96.Lt(&sqrtPriceNextX96) {
				sqrtPriceTargetX96 = &sqrtPriceLimitX96
			} else {
				sqrtPriceTargetX96 = &sqrtPriceNextX96
			}
		}

		newSqrtPriceX96, stepAmountIn, stepAmountOut, stepFeeAmount := v4ComputeSwapStepU256(
			&sqrtPriceX96, sqrtPriceTargetX96, &liquidity, &absRemaining, swapFee,
		)
		sqrtPriceX96.Set(&newSqrtPriceX96)

		var consumed uint256.Int
		consumed.Add(&stepAmountIn, &stepFeeAmount)
		absRemaining.Sub(&absRemaining, &consumed)
		amountCalculated.Add(&amountCalculated, &stepAmountOut)

		if sqrtPriceX96.Eq(&sqrtPriceNextX96) {
			if initialized {
				liquidityNet, err := v4GetTickLiquidityNetBytes(readBytes, ticksSlot, tickNext)
				if err != nil {
					return uint256.Int{}, err
				}
				if zeroForOne {
					liquidity.Sub(&liquidity, &liquidityNet)
				} else {
					liquidity.Add(&liquidity, &liquidityNet)
				}
			}
			if zeroForOne {
				tick = tickNext - 1
			} else {
				tick = tickNext
			}
		} else {
			var origPrice uint256.Int
			origPrice.SetFromBig(state.SqrtPriceX96)
			if !sqrtPriceX96.Eq(&origPrice) {
				tick = getTickAtSqrtRatioU256(&sqrtPriceX96)
			}
		}
	}

	// Apply ArenaHook fee
	if state.HookFeePpm > 0 && !amountCalculated.IsZero() {
		var feePpm, maxFee uint256.Int
		feePpm.SetUint64(uint64(state.HookFeePpm))
		maxFee.SetUint64(arenaHookMaxFee)
		feeAmt := mulDivU256(&amountCalculated, &feePpm, &maxFee)
		amountCalculated.Sub(&amountCalculated, &feeAmt)
	}

	return amountCalculated, nil
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

func v4NextInitTickBytes(
	reader BytesStateReader, bitmapSlot [32]byte, tick int32, tickSpacing int32, lte bool,
) (int32, bool, error) {
	compressed := v4Compress(tick, tickSpacing)

	if lte {
		wordPos, bitPos := v4Position(compressed)
		slotKey := cachedKeccakSlotBytes(int64(wordPos), bitmapSlot)
		data, err := reader(v4PoolManagerAddr, slotKey)
		if err != nil {
			return 0, false, err
		}
		var word, mask, masked uint256.Int
		word.SetBytes32(data[:])
		mask.Lsh(uint256.NewInt(1), uint(bitPos)+1)
		mask.SubUint64(&mask, 1)
		masked.And(&word, &mask)

		if !masked.IsZero() {
			msb := uint8(masked.BitLen() - 1)
			next := (compressed - int32(uint32(bitPos)-uint32(msb))) * tickSpacing
			return next, true, nil
		}
		next := (compressed - int32(bitPos)) * tickSpacing
		return next, false, nil
	}

	compressed++
	wordPos, bitPos := v4Position(compressed)
	slotKey := cachedKeccakSlotBytes(int64(wordPos), bitmapSlot)
	data, err := reader(v4PoolManagerAddr, slotKey)
	if err != nil {
		return 0, false, err
	}
	var word, sub, mask, masked uint256.Int
	word.SetBytes32(data[:])
	sub.Lsh(uint256.NewInt(1), uint(bitPos))
	sub.SubUint64(&sub, 1)
	mask.Not(&sub)
	mask.And(&mask, u256MaxU)
	masked.And(&word, &mask)

	if !masked.IsZero() {
		lsb := v4LsbU256(&masked)
		next := (compressed + int32(uint32(lsb)-uint32(bitPos))) * tickSpacing
		return next, true, nil
	}
	next := (compressed + int32(255-uint32(bitPos))) * tickSpacing
	return next, false, nil
}

func v4GetTickLiquidityNetBytes(reader BytesStateReader, ticksSlot [32]byte, tick int32) (uint256.Int, error) {
	slotKey := cachedKeccakSlotBytes(int64(tick), ticksSlot)
	data, err := reader(v4PoolManagerAddr, slotKey)
	if err != nil {
		return uint256.Int{}, err
	}
	var val uint256.Int
	val.SetBytes32(data[:])
	val.Rsh(&val, 128)
	val.And(&val, mask128U256)

	if val[1]>>(63)&1 != 0 {
		var highMask uint256.Int
		highMask.Lsh(u256MaxU, 128)
		val.Or(&val, &highMask)
	}
	return val, nil
}

func v4LsbU256(x *uint256.Int) uint8 {
	for w := 0; w < 4; w++ {
		if x[w] != 0 {
			for b := uint8(0); b < 64; b++ {
				if x[w]&(1<<b) != 0 {
					return uint8(w)*64 + b
				}
			}
		}
	}
	return 0
}
