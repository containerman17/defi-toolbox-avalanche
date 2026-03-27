package formulas

import (
	"fmt"
	"math/big"

	"github.com/ava-labs/libevm/crypto"

)

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


func algebraReadGlobalState(read StateReader, poolAddress string) (*big.Int, int32, uint32, uint32, error) {
	data, err := read(poolAddress, algebraSlotGlobalState)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("globalState slot: %w", err)
	}
	val := new(big.Int).SetBytes(data[:])

	// bits [0:160] = sqrtPriceX96
	mask160 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 160), big.NewInt(1))
	sqrtPrice := new(big.Int).And(val, mask160)
	if sqrtPrice.Sign() == 0 {
		return nil, 0, 0, 0, fmt.Errorf("zero sqrtPrice in storage")
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

	// bits [208:224] = communityFee (uint16) — skip pluginConfig at [200:208]
	communityFee := uint32(new(big.Int).Rsh(val, 208).Int64() & 0xFFFF)

	return sqrtPrice, tick, fee, communityFee, nil
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
	currentPrice, _, fee, _, err := algebraReadGlobalState(read, poolAddress)
	if err != nil {
		return nil, err
	}

	// Read packed slot 9: liquidity, prevTickGlobal, nextTickGlobal
	currentLiquidity, prevInitializedTick, nextInitializedTick, err := algebraReadPackedSlot(read, poolAddress)
	if err != nil {
		return nil, err
	}

	var limitSqrtPrice *big.Int
	if zeroForOne {
		limitSqrtPrice = new(big.Int).Add(algebraMinSqrtRatio, big.NewInt(1))
	} else {
		limitSqrtPrice = new(big.Int).Sub(algebraMaxSqrtRatio, big.NewInt(1))
	}

	amountRemaining := new(big.Int).Set(amountIn)
	amountOut := new(big.Int)

	for amountRemaining.Sign() > 0 && currentPrice.Cmp(limitSqrtPrice) != 0 {
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
			if currentLiquidity.Sign() <= 0 {
				// Out of liquidity — EVM would revert
				return big.NewInt(0), nil
			}
		} else if resultPrice.Cmp(currentPrice) != 0 {
			break
		}

		currentPrice = resultPrice
	}

	return amountOut, nil
}





func algebraDecodeInt24(data []byte) int32 {
	if len(data) < 32 {
		return 0
	}
	negative := data[0]&0x80 != 0
	val := int32(data[29])<<16 | int32(data[30])<<8 | int32(data[31])
	if negative {
		val = val | ^0xFFFFFF
	} else {
		val = val & 0xFFFFFF
	}
	return val
}

func algebraDecodeInt128(data []byte) *big.Int {
	if len(data) < 32 {
		return big.NewInt(0)
	}
	negative := data[0]&0x80 != 0
	val := new(big.Int).SetBytes(data[0:32])
	if negative {
		maxU256 := new(big.Int).Lsh(big.NewInt(1), 256)
		val.Sub(val, maxU256)
	}
	return val
}

func algebraEncodeInt24(tick int32) []byte {
	if tick < 0 {
		val := new(big.Int).SetInt64(int64(tick))
		maxU256 := new(big.Int).Lsh(big.NewInt(1), 256)
		val.Add(val, maxU256)
		b := val.Bytes()
		padded := make([]byte, 32)
		copy(padded[32-len(b):], b)
		return padded
	}
	padded := make([]byte, 32)
	padded[29] = byte(tick >> 16)
	padded[30] = byte(tick >> 8)
	padded[31] = byte(tick)
	return padded
}
