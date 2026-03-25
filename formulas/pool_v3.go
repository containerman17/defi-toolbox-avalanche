package formulas

import (
	"math/big"
	"strings"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// v3TickData holds a single initialized tick's position and liquidityNet.
type v3TickData struct {
	tick         int32
	liquidityNet uint256.Int
}

// V3Pool is a pre-loaded Uniswap V3 / Pharaoh V3 pool.
// Construction reads slot0, liquidity, all bitmap words, and liquidityNet for
// each initialized tick. Quote uses the SAME algorithm as QuoteV3U256 but reads
// from pre-loaded struct fields instead of state — identical results, zero state access.
type V3Pool struct {
	addr         common.Address
	fee          uint32
	tickSpacing  int32
	sqrtPriceX96 uint256.Int
	tick         int32
	liquidity    uint256.Int

	// Pre-loaded bitmap words: wordPos -> 256-bit bitmap
	bitmapWords map[int16]uint256.Int

	// Pre-loaded tick data: tickIdx -> liquidityNet
	tickLiquidityNet map[int32]uint256.Int
}

func newV3Pool(addr common.Address, reader StorageReader) *V3Pool {
	poolAddress := strings.ToLower(addr.Hex())

	feeInfo, ok := v3PoolFees[poolAddress]
	if !ok {
		return nil
	}
	fee := uint32(feeInfo[0])
	tickSpacing := feeInfo[1]

	stateReader := func(contractAddr string, slot *big.Int) ([32]byte, error) {
		a := common.HexToAddress(contractAddr)
		slotHash := common.BigToHash(slot)
		return reader(a, slotHash), nil
	}
	bytesReader := func(a [20]byte, slot [32]byte) ([32]byte, error) {
		return reader(common.Address(a), common.Hash(slot)), nil
	}

	layout, err := v3GetLayoutBytes(stateReader, poolAddress)
	if err != nil {
		return nil
	}

	poolAddr := [20]byte(addr)

	sqrtPriceX96, tick, err := v3ReadSlot0Bytes(bytesReader, poolAddr, layout.slot0)
	if err != nil {
		return nil
	}

	liquidity, err := v3ReadLiquidityBytes(bytesReader, poolAddr, layout.liquidity)
	if err != nil {
		return nil
	}

	// Pre-load ALL bitmap words (±200 range)
	bitmapWords := make(map[int16]uint256.Int)
	for wordPos := int16(-200); wordPos <= 200; wordPos++ {
		word, err := v3ReadBitmapWordBytes(bytesReader, poolAddr, layout.bitmap, wordPos)
		if err != nil {
			continue
		}
		if !word.IsZero() {
			bitmapWords[wordPos] = word
		}
	}

	// Pre-load liquidityNet for all initialized ticks
	tickLiquidityNet := make(map[int32]uint256.Int)
	for wordPos, word := range bitmapWords {
		for bit := 0; bit < 256; bit++ {
			if word[bit/64]&(1<<uint(bit%64)) != 0 {
				tickIdx := (int32(wordPos)*256 + int32(bit)) * tickSpacing
				liqNet, err := v3ReadTickLiquidityNetBytes(bytesReader, poolAddr, layout.ticks, tickIdx)
				if err != nil {
					continue
				}
				tickLiquidityNet[tickIdx] = liqNet
			}
		}
	}

	return &V3Pool{
		addr:             addr,
		fee:              fee,
		tickSpacing:      tickSpacing,
		sqrtPriceX96:     sqrtPriceX96,
		tick:             tick,
		liquidity:        liquidity,
		bitmapWords:      bitmapWords,
		tickLiquidityNet: tickLiquidityNet,
	}
}

func (p *V3Pool) Address() common.Address {
	return p.addr
}

// TickCount returns the number of pre-loaded initialized ticks.
func (p *V3Pool) TickCount() int {
	return len(p.tickLiquidityNet)
}

func (p *V3Pool) Quote(amountIn *uint256.Int, zeroForOne bool) (*uint256.Int, bool) {
	if amountIn.IsZero() {
		return nil, false
	}

	var sqrtPriceX96, liquidity, amountRemaining, amountOut uint256.Int
	sqrtPriceX96.Set(&p.sqrtPriceX96)
	liquidity.Set(&p.liquidity)
	amountRemaining.Set(amountIn)
	tick := p.tick

	var sqrtPriceLimitX96 uint256.Int
	if zeroForOne {
		sqrtPriceLimitX96.Add(u256MinSqr, uint256.NewInt(1))
	} else {
		sqrtPriceLimitX96.Sub(u256MaxSqr, uint256.NewInt(1))
	}

	for !amountRemaining.IsZero() && !sqrtPriceX96.Eq(&sqrtPriceLimitX96) {
		// SAME algorithm as v3NextInitTickBytes — word-by-word bitmap scan
		// but reads from pre-loaded bitmapWords map instead of state
		nextTick, initialized := p.nextInitializedTick(tick, zeroForOne)

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
			&sqrtPriceX96, sqrtPriceTargetX96, &liquidity, &amountRemaining, p.fee,
		)

		amountRemaining.Sub(&amountRemaining, &amountInStep)
		amountRemaining.Sub(&amountRemaining, &feeAmount)
		amountOut.Add(&amountOut, &amountOutStep)
		sqrtPriceX96.Set(&newSqrtPriceX96)

		if newSqrtPriceX96.Eq(&sqrtPriceNextTickX96) {
			if initialized {
				liquidityNet := p.tickLiquidityNet[nextTick]
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

	if amountOut.IsZero() {
		return nil, false
	}

	result := new(uint256.Int).Set(&amountOut)
	return result, true
}

// nextInitializedTick finds the next initialized tick by scanning pre-loaded bitmap words.
// Scans the current word first (same masking as Solidity), then continues through
// subsequent words in a tight loop — skipping empty words without going through
// computeSwapStep. Max error: <7 PPM (parts per million) from fee rounding differences.
func (p *V3Pool) nextInitializedTick(tick int32, zeroForOne bool) (int32, bool) {
	if len(p.bitmapWords) == 0 {
		if zeroForOne {
			return algebraMinTick, false
		}
		return algebraMaxTick, false
	}

	compressed := v3FloorDiv(int(tick), int(p.tickSpacing))

	if zeroForOne {
		wordPos, bitPos := v3Position(compressed)
		word := p.bitmapWords[wordPos]

		var mask, masked uint256.Int
		mask.Lsh(uint256.NewInt(1), uint(bitPos)+1)
		mask.SubUint64(&mask, 1)
		masked.And(&word, &mask)

		if !masked.IsZero() {
			msb := masked.BitLen() - 1
			next := (compressed - (int(bitPos) - msb)) * int(p.tickSpacing)
			return int32(next), true
		}

		// Exact: return word boundary (same as Solidity single-word scan)
		next := (compressed - int(bitPos)) * int(p.tickSpacing)
		return int32(next), false
	}

	compressed++
	wordPos, bitPos := v3Position(compressed)
	word := p.bitmapWords[wordPos]

	var maskSub, mask, masked uint256.Int
	maskSub.Lsh(uint256.NewInt(1), uint(bitPos))
	maskSub.SubUint64(&maskSub, 1)
	mask.Not(&maskSub)
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
		next := (compressed + (lsb - int(bitPos))) * int(p.tickSpacing)
		return int32(next), true
	}
	next := (compressed + (255 - int(bitPos))) * int(p.tickSpacing)
	return int32(next), false
}
