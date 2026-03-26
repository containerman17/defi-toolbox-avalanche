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
	}
	v3LayoutPangolin = v3Layout{
		slot0:     big.NewInt(5),
		liquidity: big.NewInt(9),
		ticks:     big.NewInt(10),
		bitmap:    big.NewInt(11),
	}
	v3LayoutPharaohV1 = func() v3Layout {
		base := new(big.Int).SetBytes(crypto.Keccak256([]byte("states.storage")))
		return v3Layout{
			slot0:     new(big.Int).Add(new(big.Int).Set(base), big.NewInt(7)),
			liquidity: new(big.Int).Add(new(big.Int).Set(base), big.NewInt(13)),
			ticks:     new(big.Int).Add(new(big.Int).Set(base), big.NewInt(14)),
			bitmap:    new(big.Int).Add(new(big.Int).Set(base), big.NewInt(15)),
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
		}
	}()
)

// v3ResolveLayout detects the storage layout for a V3 pool by trying slot0 reads.
func v3ResolveLayout(read StateReader, poolAddress string) (*v3Layout, error) {
	layouts := []*v3Layout{&v3LayoutStandard, &v3LayoutProxy, &v3LayoutPangolin}
	if pharaohV3Pools[poolAddress] {
		layouts = []*v3Layout{&v3LayoutPharaohV1, &v3LayoutPharaohV2}
	}
	for _, l := range layouts {
		_, _, err := v3ReadSlot0(read, poolAddress, l.slot0)
		if err == nil {
			return l, nil
		}
	}
	return nil, fmt.Errorf("no valid V3 layout found for %s", poolAddress)
}

// QuoteV3 simulates a V3 exact-input swap. Pure function — all state is read
// through the reader callback. Bitmap words and ticks are fetched lazily.
func QuoteV3(read StateReader, poolAddress string, amountIn *big.Int, zeroForOne bool) (*big.Int, error) {
	if amountIn.Sign() <= 0 {
		return big.NewInt(0), nil
	}

	feeInfo, ok := v3PoolFees[poolAddress]
	if !ok {
		return nil, fmt.Errorf("pool %s not in v3 fee registry", poolAddress)
	}
	fee := uint32(feeInfo[0])
	tickSpacing := feeInfo[1]

	layout, err := v3ResolveLayout(read, poolAddress)
	if err != nil {
		return nil, err
	}

	sqrtPriceX96, tick, err := v3ReadSlot0(read, poolAddress, layout.slot0)
	if err != nil {
		return nil, fmt.Errorf("slot0: %w", err)
	}

	liquidity, err := v3ReadLiquidity(read, poolAddress, layout.liquidity)
	if err != nil {
		return nil, fmt.Errorf("liquidity: %w", err)
	}

	amountRemaining := new(big.Int).Set(amountIn)
	amountOut := big.NewInt(0)

	var sqrtPriceLimitX96 *big.Int
	if zeroForOne {
		sqrtPriceLimitX96 = new(big.Int).Add(algebraMinSqrtRatio, big.NewInt(1))
	} else {
		sqrtPriceLimitX96 = new(big.Int).Sub(algebraMaxSqrtRatio, big.NewInt(1))
	}

	for amountRemaining.Sign() > 0 && sqrtPriceX96.Cmp(sqrtPriceLimitX96) != 0 {
		nextTick, initialized, err := v3NextInitializedTickWithinOneWord(
			read, poolAddress, layout.bitmap, tick, tickSpacing, zeroForOne,
		)
		if err != nil {
			return nil, err
		}

		if nextTick < algebraMinTick {
			nextTick = algebraMinTick
		}
		if nextTick > algebraMaxTick {
			nextTick = algebraMaxTick
		}

		sqrtPriceNextTickX96 := getSqrtRatioAtTick(nextTick)

		var sqrtPriceTargetX96 *big.Int
		if zeroForOne {
			if sqrtPriceNextTickX96.Cmp(sqrtPriceLimitX96) < 0 {
				sqrtPriceTargetX96 = sqrtPriceLimitX96
			} else {
				sqrtPriceTargetX96 = sqrtPriceNextTickX96
			}
		} else {
			if sqrtPriceNextTickX96.Cmp(sqrtPriceLimitX96) > 0 {
				sqrtPriceTargetX96 = sqrtPriceLimitX96
			} else {
				sqrtPriceTargetX96 = sqrtPriceNextTickX96
			}
		}

		newSqrtPriceX96, amountInStep, amountOutStep, feeAmount := computeSwapStep(
			sqrtPriceX96, sqrtPriceTargetX96, liquidity, amountRemaining, fee,
		)

		amountRemaining.Sub(amountRemaining, amountInStep)
		amountRemaining.Sub(amountRemaining, feeAmount)
		amountOut.Add(amountOut, amountOutStep)
		sqrtPriceX96 = newSqrtPriceX96

		if newSqrtPriceX96.Cmp(sqrtPriceNextTickX96) == 0 {
			if initialized {
				liquidityNet, err := v3ReadTickLiquidityNet(read, poolAddress, layout.ticks, nextTick)
				if err != nil {
					return nil, err
				}
				if zeroForOne {
					liquidity.Sub(liquidity, liquidityNet)
				} else {
					liquidity.Add(liquidity, liquidityNet)
				}
			}
			if zeroForOne {
				tick = nextTick - 1
			} else {
				tick = nextTick
			}
		} else {
			tick = getTickAtSqrtRatio(sqrtPriceX96)
		}
	}

	return amountOut, nil
}

// v3NextInitializedTickWithinOneWord replicates V3's bitmap search with lazy reads.
func v3NextInitializedTickWithinOneWord(
	read StateReader, poolAddress string, bitmapSlot *big.Int,
	tick int32, tickSpacing int32, zeroForOne bool,
) (int32, bool, error) {
	compressed := v3FloorDiv(int(tick), int(tickSpacing))

	if zeroForOne {
		wordPos, bitPos := v3Position(compressed)
		word, err := v3ReadBitmapWord(read, poolAddress, bitmapSlot, wordPos)
		if err != nil {
			return 0, false, err
		}

		mask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(bitPos)+1), big.NewInt(1))
		masked := new(big.Int).And(word, mask)

		if masked.Sign() != 0 {
			msb := masked.BitLen() - 1
			next := (compressed - (int(bitPos) - msb)) * int(tickSpacing)
			return int32(next), true, nil
		}
		next := (compressed - int(bitPos)) * int(tickSpacing)
		return int32(next), false, nil
	}

	compressed++
	wordPos, bitPos := v3Position(compressed)
	word, err := v3ReadBitmapWord(read, poolAddress, bitmapSlot, wordPos)
	if err != nil {
		return 0, false, err
	}

	mask := new(big.Int).Not(new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(bitPos)), big.NewInt(1)))
	mask.And(mask, maxUint256)
	masked := new(big.Int).And(word, mask)

	if masked.Sign() != 0 {
		lsb := 0
		for masked.Bit(lsb) == 0 {
			lsb++
		}
		next := (compressed + (lsb - int(bitPos))) * int(tickSpacing)
		return int32(next), true, nil
	}
	next := (compressed + (255 - int(bitPos))) * int(tickSpacing)
	return int32(next), false, nil
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
	return sqrtPriceX96, tick, nil
}

func v3ReadLiquidity(read StateReader, poolAddress string, slot *big.Int) (*big.Int, error) {
	data, err := read(poolAddress, slot)
	if err != nil {
		return nil, err
	}
	liq := new(big.Int).SetBytes(data[:])
	mask128 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	liq.And(liq, mask128)
	return liq, nil
}

func v3ReadBitmapWord(read StateReader, poolAddress string, mappingSlot *big.Int, wordPos int16) (*big.Int, error) {
	keyBytes := make([]byte, 64)
	wp := new(big.Int).SetInt64(int64(wordPos))
	if wp.Sign() < 0 {
		wp.Add(wp, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	wp.FillBytes(keyBytes[0:32])
	mappingSlot.FillBytes(keyBytes[32:64])
	slotKey := new(big.Int).SetBytes(crypto.Keccak256(keyBytes))

	data, err := read(poolAddress, slotKey)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(data[:]), nil
}

func v3ReadTickLiquidityNet(read StateReader, poolAddress string, mappingSlot *big.Int, tickIdx int32) (*big.Int, error) {
	keyBytes := make([]byte, 64)
	ti := new(big.Int).SetInt64(int64(tickIdx))
	if ti.Sign() < 0 {
		ti.Add(ti, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	ti.FillBytes(keyBytes[0:32])
	mappingSlot.FillBytes(keyBytes[32:64])
	baseSlot := new(big.Int).SetBytes(crypto.Keccak256(keyBytes))

	// V3 Tick.Info struct packs liquidityGross (uint128) and liquidityNet (int128)
	// into a single storage slot: liquidityGross in lower 128 bits, liquidityNet
	// in upper 128 bits of slot+0.
	data, err := read(poolAddress, baseSlot)
	if err != nil {
		return nil, err
	}

	val := new(big.Int).SetBytes(data[:])
	// Extract upper 128 bits (liquidityNet)
	val.Rsh(val, 128)
	mask128 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	val.And(val, mask128)
	if val.Bit(127) == 1 {
		val.Sub(val, new(big.Int).Lsh(big.NewInt(1), 128))
	}
	return val, nil
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
