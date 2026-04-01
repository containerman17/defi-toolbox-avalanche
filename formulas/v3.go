package formulas

import (
	"fmt"
	"math/big"

	"github.com/ava-labs/libevm/crypto"
)

// Uniswap V3 concentrated liquidity swap formula.
//
// QuoteV3 is a pure function: it takes a StateReader callback and fetches
// bitmap words and ticks lazily during the swap, exactly as Solidity does.
// The caller controls caching via the StateReader implementation.
//
// Five storage layouts supported (auto-detected):
//   Standard V3:   slot0=0, liquidity=4, ticks=5, bitmap=6
//   Proxy V3:      slot0=4, liquidity=9, ticks=10, bitmap=11
//   Pangolin V3:   slot0=5, liquidity=9, ticks=10, bitmap=11
//   Pharaoh V3 v1: ERC-7201 keccak256("states.storage") + offsets
//   Pharaoh V3 v2: ERC-7201 keccak256("pool.storage") + offsets

// v3Layout holds storage slot offsets for V3-compatible pools.
type v3Layout struct {
	slot0     *big.Int
	liquidity *big.Int
	ticks     *big.Int // mapping root
	bitmap    *big.Int // mapping root
	feeSlot   *big.Int // non-nil for pools with dynamic/mutable fees (e.g. RamsesV3)
	heavyGas  bool     // true for PangolinV3/PharaohV3 pools with extra per-tick overhead
}

var (
	v3LayoutStandard = v3Layout{
		slot0:     big.NewInt(0),
		liquidity: big.NewInt(4),
		ticks:     big.NewInt(5),
		bitmap:    big.NewInt(6),
	}
	v3LayoutProxy = v3Layout{
		slot0:     big.NewInt(4),
		liquidity: big.NewInt(9),
		ticks:     big.NewInt(10),
		bitmap:    big.NewInt(11),
		heavyGas:  true, // PangolinV3: extra reward tracking per tick crossing
	}
	v3LayoutPangolin = v3Layout{
		slot0:     big.NewInt(5),
		liquidity: big.NewInt(9),
		ticks:     big.NewInt(10),
		bitmap:    big.NewInt(11),
		heavyGas:  true, // PangolinV3: extra reward tracking per tick crossing
	}
	// PangolinV3Pool with RewardSlot between slot0 and feeGrowthGlobal0X128.
	// The RewardSlot shifts liquidity/ticks/bitmap each by +1.
	v3LayoutPangolinReward = v3Layout{
		slot0:     big.NewInt(5),
		liquidity: big.NewInt(10),
		ticks:     big.NewInt(11),
		bitmap:    big.NewInt(12),
		heavyGas:  true, // PangolinV3: extra reward tracking per tick crossing
	}
	v3LayoutPharaohV1 = func() v3Layout {
		base := new(big.Int).SetBytes(crypto.Keccak256([]byte("states.storage")))
		return v3Layout{
			slot0:     new(big.Int).Add(new(big.Int).Set(base), big.NewInt(7)),
			liquidity: new(big.Int).Add(new(big.Int).Set(base), big.NewInt(13)),
			ticks:     new(big.Int).Add(new(big.Int).Set(base), big.NewInt(14)),
			bitmap:    new(big.Int).Add(new(big.Int).Set(base), big.NewInt(15)),
			heavyGas:  true, // PharaohV3: extra overhead per tick crossing
		}
	}()
	v3LayoutPharaohV2 = func() v3Layout {
		raw := new(big.Int).SetBytes(crypto.Keccak256([]byte("pool.storage")))
		raw.Sub(raw, big.NewInt(1))
		rawBytes := make([]byte, 32)
		raw.FillBytes(rawBytes)
		derived := new(big.Int).SetBytes(crypto.Keccak256(rawBytes))
		mask := new(big.Int).SetBytes([]byte{0xff})
		derived.AndNot(derived, mask)
		return v3Layout{
			slot0:     new(big.Int).Set(derived),
			liquidity: new(big.Int).Add(new(big.Int).Set(derived), big.NewInt(8)),
			ticks:     new(big.Int).Add(new(big.Int).Set(derived), big.NewInt(9)),
			bitmap:    new(big.Int).Add(new(big.Int).Set(derived), big.NewInt(10)),
			feeSlot:   new(big.Int).Add(new(big.Int).Set(derived), big.NewInt(2)), // PoolState.fee after Slot0(2 slots)
			heavyGas:  true, // PharaohV3: extra overhead per tick crossing
		}
	}()
)

// v3ResolveLayout detects the storage layout for a V3 pool by trying slot0 reads.
// v3ReadSlot0 validates sqrtPrice range AND tick/sqrtPrice consistency, which
// prevents false positives from non-slot0 storage data (e.g. maxLiquidityPerTick).
func v3ResolveLayout(read StateReader, poolAddress string) (*v3Layout, error) {
	layouts := []*v3Layout{&v3LayoutStandard, &v3LayoutProxy, &v3LayoutPangolinReward, &v3LayoutPangolin}
	if pharaohV3Pools[poolAddress] {
		// Try V2 (pool.storage) first: migrated pools have live data at V2 positions
		// and stale data at V1 positions that can pass validation. V2 positions are
		// zero for non-migrated pools, so V2 detection fails cleanly → falls to V1.
		layouts = []*v3Layout{&v3LayoutPharaohV2, &v3LayoutPharaohV1}
	}
	for _, l := range layouts {
		_, _, err := v3ReadSlot0(read, poolAddress, l.slot0)
		if err == nil {
			return l, nil
		}
	}
	return nil, fmt.Errorf("no valid V3 layout found for %s", poolAddress)
}

// --- Storage readers (pure functions taking StateReader) ---

func v3ReadSlot0(read StateReader, poolAddress string, slot *big.Int) (*big.Int, int32, error) {
	data, err := read(poolAddress, slot)
	if err != nil {
		return nil, 0, err
	}
	slot0Val := new(big.Int).SetBytes(data[:])
	mask160 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 160), big.NewInt(1))
	sqrtPriceX96 := new(big.Int).And(slot0Val, mask160)
	if sqrtPriceX96.Sign() == 0 {
		return nil, 0, fmt.Errorf("zero sqrtPrice in storage")
	}
	// Validate sqrtPrice is in the valid V3 range [MIN_SQRT_RATIO, MAX_SQRT_RATIO].
	// This prevents false-positive layout detection when random storage data at the
	// wrong ERC-7201 offset happens to have non-zero lower 160 bits.
	if sqrtPriceX96.Cmp(algebraMinSqrtRatio) < 0 || sqrtPriceX96.Cmp(algebraMaxSqrtRatio) > 0 {
		return nil, 0, fmt.Errorf("sqrtPrice %s out of valid range", sqrtPriceX96)
	}
	highBits := new(big.Int).Rsh(slot0Val, 160)
	if highBits.Sign() == 0 {
		return nil, 0, fmt.Errorf("slot0 looks like address (no tick/obs data)")
	}
	tickRaw := new(big.Int).And(highBits, big.NewInt(0xFFFFFF))
	tick := int32(tickRaw.Int64())
	if tick >= 1<<23 {
		tick -= 1 << 24
	}
	// Validate tick is in valid range
	if tick < algebraMinTick || tick > algebraMaxTick {
		return nil, 0, fmt.Errorf("tick %d out of valid range [%d, %d]", tick, algebraMinTick, algebraMaxTick)
	}
	// Validate tick/sqrtPrice consistency: getSqrtRatioAtTick(tick) <= sqrtPrice < getSqrtRatioAtTick(tick+1).
	// This prevents false positives where non-slot0 data (e.g. maxLiquidityPerTick)
	// happens to have lower 160 bits in the valid sqrtPrice range but with garbage upper bits.
	tickLowerSqrt := getSqrtRatioAtTick(tick)
	if sqrtPriceX96.Cmp(tickLowerSqrt) < 0 {
		return nil, 0, fmt.Errorf("sqrtPrice %s below tick %d lower bound", sqrtPriceX96, tick)
	}
	if tick < algebraMaxTick {
		tickUpperSqrt := getSqrtRatioAtTick(tick + 1)
		if sqrtPriceX96.Cmp(tickUpperSqrt) >= 0 {
			return nil, 0, fmt.Errorf("sqrtPrice %s at or above tick %d upper bound", sqrtPriceX96, tick)
		}
	}
	return sqrtPriceX96, tick, nil
}

// --- Helpers ---

func v3Position(compressed int) (int16, uint8) {
	wordPos := int16(compressed >> 8)
	bitPos := uint8(compressed - int(wordPos)*256)
	return wordPos, bitPos
}

func v3FloorDiv(a, b int) int {
	result := a / b
	if (a^b) < 0 && result*b != a {
		result--
	}
	return result
}

// --- computeSwapStep (V3-specific) ---

func computeSwapStep(
	sqrtRatioCurrentX96, sqrtRatioTargetX96, liquidity, amountRemaining *big.Int,
	feePips uint32,
) (sqrtRatioNextX96, amountIn, amountOut, feeAmount *big.Int) {
	zeroForOne := sqrtRatioCurrentX96.Cmp(sqrtRatioTargetX96) >= 0
	feeDenom := big.NewInt(1_000_000)
	feeComplement := new(big.Int).Sub(feeDenom, big.NewInt(int64(feePips)))
	amountRemainingLessFee := mulDiv(amountRemaining, feeComplement, feeDenom)

	if zeroForOne {
		amountIn = sGetAmount0Delta(sqrtRatioTargetX96, sqrtRatioCurrentX96, liquidity, true)
	} else {
		amountIn = sGetAmount1Delta(sqrtRatioCurrentX96, sqrtRatioTargetX96, liquidity, true)
	}

	if amountRemainingLessFee.Cmp(amountIn) >= 0 {
		sqrtRatioNextX96 = new(big.Int).Set(sqrtRatioTargetX96)
	} else {
		sqrtRatioNextX96 = v3GetNextPriceFromInput(sqrtRatioCurrentX96, liquidity, amountRemainingLessFee, zeroForOne)
	}

	isMax := sqrtRatioTargetX96.Cmp(sqrtRatioNextX96) == 0
	if zeroForOne {
		if !isMax {
			amountIn = sGetAmount0Delta(sqrtRatioNextX96, sqrtRatioCurrentX96, liquidity, true)
		}
		amountOut = sGetAmount1Delta(sqrtRatioNextX96, sqrtRatioCurrentX96, liquidity, false)
	} else {
		if !isMax {
			amountIn = sGetAmount1Delta(sqrtRatioCurrentX96, sqrtRatioNextX96, liquidity, true)
		}
		amountOut = sGetAmount0Delta(sqrtRatioCurrentX96, sqrtRatioNextX96, liquidity, false)
	}

	if !isMax {
		feeAmount = new(big.Int).Sub(amountRemaining, amountIn)
	} else {
		feeAmount = mulDivRoundingUp(amountIn, big.NewInt(int64(feePips)), feeComplement)
	}
	return
}

func v3GetNextPriceFromInput(sqrtPX96, liquidity, amountIn *big.Int, zeroForOne bool) *big.Int {
	if zeroForOne {
		product := new(big.Int).Mul(amountIn, sqrtPX96)
		if product.BitLen() > 256 {
			return v3GetNextPriceFromInputOverflow(sqrtPX96, liquidity, amountIn, true)
		}
	}
	return sGetNextSqrtPriceFromInput(sqrtPX96, liquidity, amountIn, zeroForOne)
}

func v3GetNextPriceFromInputOverflow(sqrtPX96, liquidity, amountIn *big.Int, zeroForOne bool) *big.Int {
	if !zeroForOne {
		return sGetNextSqrtPriceFromInput(sqrtPX96, liquidity, amountIn, false)
	}
	if amountIn.Sign() == 0 {
		return new(big.Int).Set(sqrtPX96)
	}
	numerator1 := new(big.Int).Lsh(liquidity, 96)
	denom := new(big.Int).Div(numerator1, sqrtPX96)
	denom.Add(denom, amountIn)
	return unsafeDivRoundingUp(numerator1, denom)
}
