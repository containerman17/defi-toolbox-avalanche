package formulas

import (
	"fmt"
	"math/big"
	"os"

	"github.com/ava-labs/libevm/crypto"

)

var algebraDebug = os.Getenv("ALGEBRA_DEBUG") == "1"

//
//

var (
	algebraSlotGlobalState = big.NewInt(2)
	algebraSlotPacked      = big.NewInt(9)
	algebraSlotTicksMap    = big.NewInt(3)
)

type AlgebraState struct {
	SqrtPrice    *big.Int // globalState.price (Q64.96)
	Tick         int32    // globalState.tick
	Fee          uint32   // globalState.lastFee (hundredths of a bip, i.e. 1e-6)
	CommunityFee uint32  // globalState.communityFee
	Liquidity    *big.Int // current active liquidity (uint128)
	PrevTick     int32    // prevTickGlobal
	NextTick     int32    // nextTickGlobal
}

type AlgebraTickData struct {
	LiquidityDelta *big.Int // int128: net liquidity change when crossing left-to-right
	PrevTick       int32
	NextTick       int32
}

var (
	algebraGlobalStateSelector    = crypto.Keccak256([]byte("globalState()"))[:4]
	algebraLiquiditySelector      = crypto.Keccak256([]byte("liquidity()"))[:4]
	algebraPrevTickGlobalSelector = crypto.Keccak256([]byte("prevTickGlobal()"))[:4]
	algebraNextTickGlobalSelector = crypto.Keccak256([]byte("nextTickGlobal()"))[:4]
	algebraTicksSelector          = crypto.Keccak256([]byte("ticks(int24)"))[:4]
)


func algebraReadGlobalState(read StateReader, poolAddress string) (*big.Int, int32, uint32, uint32, uint8, error) {
	data, err := read(poolAddress, algebraSlotGlobalState)
	if err != nil {
		return nil, 0, 0, 0, 0, fmt.Errorf("globalState slot: %w", err)
	}
	val := new(big.Int).SetBytes(data[:])

	// bits [0:160] = sqrtPriceX96
	mask160 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 160), big.NewInt(1))
	sqrtPrice := new(big.Int).And(val, mask160)
	if sqrtPrice.Sign() == 0 {
		return nil, 0, 0, 0, 0, fmt.Errorf("zero sqrtPrice in storage")
	}

	// bits [160:184] = tick (int24)
	tickRaw := new(big.Int).Rsh(val, 160)
	tickRaw.And(tickRaw, big.NewInt(0xFFFFFF))
	tick := int32(tickRaw.Int64())
	if tick >= 1<<23 {
		tick -= 1 << 24
	}

	// bits [184:200] = lastFee (uint16)
	fee := uint32(new(big.Int).Rsh(val, 184).Int64() & 0xFFFF)

	// bits [200:208] = pluginConfig (uint8)
	pluginConfig := uint8(new(big.Int).Rsh(val, 200).Int64() & 0xFF)

	// bits [208:224] = communityFee (uint16)
	communityFee := uint32(new(big.Int).Rsh(val, 208).Int64() & 0xFFFF)

	return sqrtPrice, tick, fee, communityFee, pluginConfig, nil
}

func algebraReadPackedSlot(read StateReader, poolAddress string) (*big.Int, int32, int32, error) {
	data, err := read(poolAddress, algebraSlotPacked)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("packed slot: %w", err)
	}
	val := new(big.Int).SetBytes(data[:])

	// bits [0:24] = nextTickGlobal (int24)
	nextRaw := int32(val.Int64() & 0xFFFFFF)
	if nextRaw >= 1<<23 {
		nextRaw -= 1 << 24
	}

	// bits [24:48] = prevTickGlobal (int24)
	prevRaw := int32(new(big.Int).Rsh(val, 24).Int64() & 0xFFFFFF)
	if prevRaw >= 1<<23 {
		prevRaw -= 1 << 24
	}

	// bits [48:176] = liquidity (uint128)
	mask128 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	liquidity := new(big.Int).Rsh(val, 48)
	liquidity.And(liquidity, mask128)

	return liquidity, prevRaw, nextRaw, nil
}

func algebraReadTick(read StateReader, poolAddress string, tickIdx int32) (*big.Int, int32, int32, error) {
	// Compute base slot: keccak256(abi.encode(tickIndex, 3))
	keyBytes := make([]byte, 64)
	ti := new(big.Int).SetInt64(int64(tickIdx))
	if ti.Sign() < 0 {
		ti.Add(ti, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	ti.FillBytes(keyBytes[0:32])
	algebraSlotTicksMap.FillBytes(keyBytes[32:64])
	baseSlot := new(big.Int).SetBytes(crypto.Keccak256(keyBytes))

	// Algebra Integral tick struct layout (TickManagement.sol):
	//   slot+0: uint256 liquidityTotal       (full 256 bits, not needed for quoting)
	//   slot+1: int128  liquidityDelta [0:128] | int24 prevTick [128:152] | int24 nextTick [152:176]
	//   slot+2: uint256 outerFeeGrowth0Token
	//   slot+3: uint256 outerFeeGrowth1Token
	// We only need slot+1.
	slot1 := new(big.Int).Add(baseSlot, big.NewInt(1))
	data1, err := read(poolAddress, slot1)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("tick %d slot+1: %w", tickIdx, err)
	}
	val1 := new(big.Int).SetBytes(data1[:])

	// liquidityDelta = lower 128 bits of slot+1
	mask128 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	liquidityDelta := new(big.Int).And(new(big.Int).Set(val1), mask128)
	// Sign extend int128
	if liquidityDelta.Bit(127) == 1 {
		liquidityDelta.Sub(liquidityDelta, new(big.Int).Lsh(big.NewInt(1), 128))
	}

	// prevTick at bits [128:152]
	prevRaw := int32(new(big.Int).Rsh(val1, 128).Int64() & 0xFFFFFF)
	if prevRaw >= 1<<23 {
		prevRaw -= 1 << 24
	}

	// nextTick at bits [152:176]
	nextRaw := int32(new(big.Int).Rsh(val1, 152).Int64() & 0xFFFFFF)
	if nextRaw >= 1<<23 {
		nextRaw -= 1 << 24
	}

	return liquidityDelta, prevRaw, nextRaw, nil
}

func QuoteAlgebraStorage(read StateReader, poolAddress string, amountIn *big.Int, zeroForOne bool) (*big.Int, error) {
	if amountIn.Sign() <= 0 {
		return big.NewInt(0), nil
	}

	// Read globalState from slot 2
	currentPrice, _, fee, _, pluginConfig, err := algebraReadGlobalState(read, poolAddress)
	if err != nil {
		return nil, err
	}

	// Read packed slot 9: liquidity, prevTickGlobal, nextTickGlobal
	currentLiquidity, prevInitializedTick, nextInitializedTick, err := algebraReadPackedSlot(read, poolAddress)
	if err != nil {
		return nil, err
	}

	// Gas-based step limit: estimate accumulated EVM gas and bail before 5M limit.
	// The swap loop costs ~22K gas per step (tick crossing) regardless of pool config.
	// However, the AFTER_SWAP plugin hook can add significant one-time overhead when
	// the pool has accumulated pending community fees — the plugin triggers fee
	// transfers + TWAP oracle updates that can cost 2-3M gas.
	//
	// We estimate afterSwap overhead by reading slot 4 (communityFeePending0).
	// When the pool has AFTER_SWAP enabled and pending fees > threshold, we
	// deduct an afterSwap penalty from the gas budget.
	//
	// Examples:
	//   Pool 0xA02E: 217 steps, 4.9M gas, afterSwap=cheap (pending=191B) → REVERTS
	//   Pool 0xC13F: 147 steps, 3.3M gas, afterSwap=cheap (pending=0)   → SUCCEEDS
	//   Pool 0x4110: 102 steps, 4.9M gas, afterSwap=2.6M (pending=29T)  → REVERTS
	const gasPerStep int64 = 22_000
	const algebraBaseGas int64 = 200_000
	const algebraGasLimit int64 = 4_800_000

	// Estimate afterSwap overhead from pending community fees in slot 4.
	// Slot 4 layout (Algebra V2 Integral AlgebraPoolBase):
	//   bits [0:104]   = communityFeePending0 (uint104)
	//   bits [104:208] = communityFeePending1 (uint104)
	//   bits [208:240] = lastFeeTransferTimestamp (uint32)
	var afterSwapGas int64
	if pluginConfig&0x02 != 0 { // AFTER_SWAP_FLAG
		data4, err4 := read(poolAddress, big.NewInt(4))
		if err4 == nil {
			val4 := new(big.Int).SetBytes(data4[:])
			mask104 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 104), big.NewInt(1))
			feePending0 := new(big.Int).And(val4, mask104)
			feePending1 := new(big.Int).And(new(big.Int).Rsh(val4, 104), mask104)
			// Graduated afterSwap gas penalty based on pending fee magnitude.
			// The afterSwap plugin cost comes from TWAP oracle catch-up + fee transfers.
			// Small pending fees (~1e12-1e18): cheap afterSwap (~200K)
			// Large pending fees (>1e18): expensive afterSwap (~2.6M from oracle catch-up)
			// Original calibration: 29T pending → 2.6M gas (pool 0x4110)
			maxPending := feePending0
			if feePending1.Cmp(maxPending) > 0 {
				maxPending = feePending1
			}
			threshold18 := new(big.Int).SetUint64(1_000_000_000_000_000_000) // 1e18
			if maxPending.Cmp(threshold18) > 0 {
				afterSwapGas = 2_600_000 // heavy: TWAP catch-up + fee transfers
			}
		}
	}
	effectiveGasLimit := algebraGasLimit - afterSwapGas

	if algebraDebug {
		fmt.Fprintf(os.Stderr, "[ALGEBRA] pool=%s z4o=%v amt=%s sqrtP=%s liq=%s prev=%d next=%d fee=%d plugin=0x%02x gasPerStep=%dK afterSwap=%dK maxSteps=%d\n",
			poolAddress, zeroForOne, amountIn.String(), currentPrice.String(), currentLiquidity.String(),
			prevInitializedTick, nextInitializedTick, fee, pluginConfig, gasPerStep/1000, afterSwapGas/1000,
			(effectiveGasLimit-algebraBaseGas)/gasPerStep)
	}

	var limitSqrtPrice *big.Int
	if zeroForOne {
		limitSqrtPrice = new(big.Int).Add(algebraMinSqrtRatio, big.NewInt(1))
	} else {
		limitSqrtPrice = new(big.Int).Sub(algebraMaxSqrtRatio, big.NewInt(1))
	}

	amountRemaining := new(big.Int).Set(amountIn)
	amountOut := new(big.Int)
	steps := 0
	for amountRemaining.Sign() > 0 && currentPrice.Cmp(limitSqrtPrice) != 0 {
		steps++
		if int64(steps)*gasPerStep+algebraBaseGas > effectiveGasLimit {
			if algebraDebug {
				fmt.Fprintf(os.Stderr, "[ALGEBRA] gas estimate %dK exceeds limit %dK at step %d (gasPerStep=%dK afterSwap=%dK), out=%s rem=%s\n",
					(int64(steps)*gasPerStep+algebraBaseGas)/1000, effectiveGasLimit/1000, steps, gasPerStep/1000, afterSwapGas/1000, amountOut.String(), amountRemaining.String())
			}
			return big.NewInt(0), nil
		}
		var nextTick int32
		if zeroForOne {
			nextTick = prevInitializedTick
		} else {
			nextTick = nextInitializedTick
		}

		nextTickPrice := getSqrtRatioAtTick(nextTick)

		var targetPrice *big.Int
		if zeroForOne {
			if nextTickPrice.Cmp(limitSqrtPrice) < 0 {
				targetPrice = limitSqrtPrice
			} else {
				targetPrice = nextTickPrice
			}
		} else {
			if nextTickPrice.Cmp(limitSqrtPrice) > 0 {
				targetPrice = limitSqrtPrice
			} else {
				targetPrice = nextTickPrice
			}
		}

		// The swap step math is identical to V3
		resultPrice, inputStep, outputStep, feeAmount := computeSwapStep(
			currentPrice, targetPrice, currentLiquidity, amountRemaining, fee,
		)

		amountRemaining.Sub(amountRemaining, inputStep)
		amountRemaining.Sub(amountRemaining, feeAmount)
		amountOut.Add(amountOut, outputStep)

		if algebraDebug && steps <= 5 {
			fmt.Fprintf(os.Stderr, "[ALGEBRA] step %d: tick=%d in=%s out=%s fee=%s rem=%s liq=%s\n",
				steps, nextTick, inputStep.String(), outputStep.String(), feeAmount.String(),
				amountRemaining.String(), currentLiquidity.String())
		}

		if resultPrice.Cmp(nextTickPrice) == 0 {
			// Read tick data from storage
			liquidityDelta, prevTick, nextTickVal, err := algebraReadTick(read, poolAddress, nextTick)
			if err != nil {
				return nil, fmt.Errorf("fetch tick %d: %w", nextTick, err)
			}

			if zeroForOne {
				liquidityDelta = new(big.Int).Neg(liquidityDelta)
				nextInitializedTick = nextTick
				prevInitializedTick = prevTick
			} else {
				prevInitializedTick = nextTick
				nextInitializedTick = nextTickVal
			}

			currentLiquidity = new(big.Int).Add(currentLiquidity, liquidityDelta)
			if algebraDebug && steps <= 5 {
				fmt.Fprintf(os.Stderr, "[ALGEBRA]   crossed tick %d: delta=%s newLiq=%s prev=%d next=%d\n",
					nextTick, liquidityDelta.String(), currentLiquidity.String(), prevTick, nextTickVal)
			}
			// Note: liquidity can legitimately be 0 between ticks (gap in LP coverage).
			// Algebra EVM continues through gaps — computeSwapStep with 0 liquidity
			// produces 0 amounts and just moves price to the next tick.
			// Negative liquidity indicates corrupt tick data — bail out.
			if currentLiquidity.Sign() < 0 {
				return big.NewInt(0), nil
			}
		} else if resultPrice.Cmp(currentPrice) != 0 {
			break
		}

		currentPrice = resultPrice
	}

	if algebraDebug {
		fmt.Fprintf(os.Stderr, "[ALGEBRA] DONE: steps=%d out=%s rem=%s\n", steps, amountOut.String(), amountRemaining.String())
	}
	return amountOut, nil
}
