package formulas

import (
	"fmt"
	"math/big"

	"github.com/ava-labs/libevm/crypto"
	"github.com/holiman/uint256"
)

// v3_u256.go — uint256 version of QuoteV3.
// Uses BytesStateReader for zero big.Int overhead in the hot path.

// v3LayoutBytes holds storage slot offsets as [32]byte for BytesStateReader.
type v3LayoutBytes struct {
	slot0     [32]byte
	liquidity [32]byte
	ticks     [32]byte
	bitmap    [32]byte
}

var (
	v3LayoutBytesCache = make(map[string]*v3LayoutBytes)
)

func v3GetLayoutBytes(read StateReader, poolAddress string) (*v3LayoutBytes, error) {
	if lb, ok := v3LayoutBytesCache[poolAddress]; ok {
		return lb, nil
	}
	layout, err := v3ResolveLayout(read, poolAddress)
	if err != nil {
		return nil, err
	}
	lb := &v3LayoutBytes{
		slot0:     bigIntTo32(layout.slot0),
		liquidity: bigIntTo32(layout.liquidity),
		ticks:     bigIntTo32(layout.ticks),
		bitmap:    bigIntTo32(layout.bitmap),
	}
	v3LayoutBytesCache[poolAddress] = lb
	return lb, nil
}

// QuoteV3U256 simulates a V3 exact-input swap using uint256 math.
// Uses the old StateReader for layout resolution, then BytesStateReader for the hot path.
func QuoteV3U256(read StateReader, readBytes BytesStateReader, poolAddr [20]byte, poolAddress string, amountIn *uint256.Int, zeroForOne bool) (uint256.Int, error) {
	if amountIn.IsZero() {
		return uint256.Int{}, nil
	}

	feeInfo, ok := v3PoolFees[poolAddress]
	if !ok {
		return uint256.Int{}, fmt.Errorf("pool %s not in v3 fee registry", poolAddress)
	}
	fee := uint32(feeInfo[0])
	tickSpacing := feeInfo[1]

	layout, err := v3GetLayoutBytes(read, poolAddress)
	if err != nil {
		return uint256.Int{}, err
	}

	sqrtPriceX96, tick, err := v3ReadSlot0Bytes(readBytes, poolAddr, layout.slot0)
	if err != nil {
		return uint256.Int{}, fmt.Errorf("slot0: %w", err)
	}

	liquidity, err := v3ReadLiquidityBytes(readBytes, poolAddr, layout.liquidity)
	if err != nil {
		return uint256.Int{}, fmt.Errorf("liquidity: %w", err)
	}

	var amountRemaining, amountOut uint256.Int
	amountRemaining.Set(amountIn)

	var sqrtPriceLimitX96 uint256.Int
	if zeroForOne {
		sqrtPriceLimitX96.Add(u256MinSqr, uint256.NewInt(1))
	} else {
		sqrtPriceLimitX96.Sub(u256MaxSqr, uint256.NewInt(1))
	}

	for !amountRemaining.IsZero() && !sqrtPriceX96.Eq(&sqrtPriceLimitX96) {
		nextTick, initialized, err := v3NextInitTickBytes(
			readBytes, poolAddr, layout.bitmap, tick, tickSpacing, zeroForOne,
		)
		if err != nil {
			return uint256.Int{}, err
		}

		if nextTick < algebraMinTick {
			nextTick = algebraMinTick
		}
		if nextTick > algebraMaxTick {
			nextTick = algebraMaxTick
		}

		sqrtPriceNextTickX96 := getSqrtRatioAtTickU256(nextTick)

		var sqrtPriceTargetX96 *uint256.Int
		if zeroForOne {
			if sqrtPriceNextTickX96.Lt(&sqrtPriceLimitX96) {
				sqrtPriceTargetX96 = &sqrtPriceLimitX96
			} else {
				sqrtPriceTargetX96 = &sqrtPriceNextTickX96
			}
		} else {
			if sqrtPriceLimitX96.Lt(&sqrtPriceNextTickX96) {
				sqrtPriceTargetX96 = &sqrtPriceLimitX96
			} else {
				sqrtPriceTargetX96 = &sqrtPriceNextTickX96
			}
		}

		newSqrtPriceX96, amountInStep, amountOutStep, feeAmount := computeSwapStepU256(
			&sqrtPriceX96, sqrtPriceTargetX96, &liquidity, &amountRemaining, fee,
		)

		amountRemaining.Sub(&amountRemaining, &amountInStep)
		amountRemaining.Sub(&amountRemaining, &feeAmount)
		amountOut.Add(&amountOut, &amountOutStep)
		sqrtPriceX96.Set(&newSqrtPriceX96)

		if newSqrtPriceX96.Eq(&sqrtPriceNextTickX96) {
			if initialized {
				liquidityNet, err := v3ReadTickLiquidityNetBytes(readBytes, poolAddr, layout.ticks, nextTick)
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
				tick = nextTick - 1
			} else {
				tick = nextTick
			}
		} else {
			tick = getTickAtSqrtRatioU256(&sqrtPriceX96)
		}
	}

	return amountOut, nil
}

// ─── Storage readers using BytesStateReader ───

func v3ReadSlot0Bytes(read BytesStateReader, addr [20]byte, slot [32]byte) (uint256.Int, int32, error) {
	data, err := read(addr, slot)
	if err != nil {
		return uint256.Int{}, 0, err
	}
	var slot0Val uint256.Int
	slot0Val.SetBytes32(data[:])

	var mask160, sqrtPriceX96 uint256.Int
	mask160.Sub(new(uint256.Int).Lsh(uint256.NewInt(1), 160), uint256.NewInt(1))
	sqrtPriceX96.And(&slot0Val, &mask160)
	if sqrtPriceX96.IsZero() {
		return uint256.Int{}, 0, fmt.Errorf("zero sqrtPrice in storage")
	}

	var highBits uint256.Int
	highBits.Rsh(&slot0Val, 160)
	if highBits.IsZero() {
		return uint256.Int{}, 0, fmt.Errorf("slot0 looks like address (no tick/obs data)")
	}

	tickRaw := highBits.Uint64() & 0xFFFFFF
	tick := int32(tickRaw)
	if tick >= 1<<23 {
		tick -= 1 << 24
	}
	return sqrtPriceX96, tick, nil
}

func v3ReadLiquidityBytes(read BytesStateReader, addr [20]byte, slot [32]byte) (uint256.Int, error) {
	data, err := read(addr, slot)
	if err != nil {
		return uint256.Int{}, err
	}
	var liq uint256.Int
	liq.SetBytes32(data[:])
	liq.And(&liq, mask128U256)
	return liq, nil
}

func v3ReadTickLiquidityNetBytes(read BytesStateReader, addr [20]byte, ticksSlot [32]byte, tickIdx int32) (uint256.Int, error) {
	baseSlot := cachedKeccakSlotBytes(int64(tickIdx), ticksSlot)

	data, err := read(addr, baseSlot)
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

func v3NextInitTickBytes(
	read BytesStateReader, addr [20]byte, bitmapSlot [32]byte,
	tick int32, tickSpacing int32, zeroForOne bool,
) (int32, bool, error) {
	compressed := v3FloorDiv(int(tick), int(tickSpacing))

	if zeroForOne {
		wordPos, bitPos := v3Position(compressed)
		word, err := v3ReadBitmapWordBytes(read, addr, bitmapSlot, wordPos)
		if err != nil {
			return 0, false, err
		}

		var mask, masked uint256.Int
		mask.Lsh(uint256.NewInt(1), uint(bitPos)+1)
		mask.SubUint64(&mask, 1)
		masked.And(&word, &mask)

		if !masked.IsZero() {
			msb := masked.BitLen() - 1
			next := (compressed - (int(bitPos) - msb)) * int(tickSpacing)
			return int32(next), true, nil
		}
		next := (compressed - int(bitPos)) * int(tickSpacing)
		return int32(next), false, nil
	}

	compressed++
	wordPos, bitPos := v3Position(compressed)
	word, err := v3ReadBitmapWordBytes(read, addr, bitmapSlot, wordPos)
	if err != nil {
		return 0, false, err
	}

	var mask, sub, masked uint256.Int
	sub.Lsh(uint256.NewInt(1), uint(bitPos))
	sub.SubUint64(&sub, 1)
	mask.Not(&sub)
	mask.And(&mask, u256MaxU)
	masked.And(&word, &mask)

	if !masked.IsZero() {
		lsb := 0
		for w := 0; w < 4; w++ {
			if masked[w] != 0 {
				for b := 0; b < 64; b++ {
					if masked[w]&(1<<uint(b)) != 0 {
						lsb = w*64 + b
						goto foundLsb
					}
				}
			}
		}
	foundLsb:
		next := (compressed + (lsb - int(bitPos))) * int(tickSpacing)
		return int32(next), true, nil
	}
	next := (compressed + (255 - int(bitPos))) * int(tickSpacing)
	return int32(next), false, nil
}

func v3ReadBitmapWordBytes(read BytesStateReader, addr [20]byte, bitmapSlot [32]byte, wordPos int16) (uint256.Int, error) {
	slotKey := cachedKeccakSlotBytes(int64(wordPos), bitmapSlot)
	data, err := read(addr, slotKey)
	if err != nil {
		return uint256.Int{}, err
	}
	var result uint256.Int
	result.SetBytes32(data[:])
	return result, nil
}

// keccak256U256 helper.
func keccak256U256(data []byte) []byte {
	return crypto.Keccak256(data)
}

// Keep big.Int-based reader functions needed by v3ResolveLayout
func v3ReadSlot0U256(read StateReader, poolAddress string, slot *big.Int) (uint256.Int, int32, error) {
	data, err := read(poolAddress, slot)
	if err != nil {
		return uint256.Int{}, 0, err
	}
	var slot0Val uint256.Int
	slot0Val.SetBytes32(data[:])

	var mask160, sqrtPriceX96 uint256.Int
	mask160.Sub(new(uint256.Int).Lsh(uint256.NewInt(1), 160), uint256.NewInt(1))
	sqrtPriceX96.And(&slot0Val, &mask160)
	if sqrtPriceX96.IsZero() {
		return uint256.Int{}, 0, fmt.Errorf("zero sqrtPrice in storage")
	}

	var highBits uint256.Int
	highBits.Rsh(&slot0Val, 160)
	if highBits.IsZero() {
		return uint256.Int{}, 0, fmt.Errorf("slot0 looks like address (no tick/obs data)")
	}

	tickRaw := highBits.Uint64() & 0xFFFFFF
	tick := int32(tickRaw)
	if tick >= 1<<23 {
		tick -= 1 << 24
	}
	return sqrtPriceX96, tick, nil
}
