package formulas

import (
	"math/big"
	"strings"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// v3PrecomputedStep holds pre-computed swap results for crossing one word boundary.
// Computed at construction time when liquidity and boundary sqrtPrices are known.
// Lossless: identical to calling computeSwapStepU256 at quote time.
type v3PrecomputedStep struct {
	sqrtPriceTarget uint256.Int // getSqrtRatioAtTick(boundaryTick)
	amountIn        uint256.Int // getAmountDelta for this crossing
	amountOut       uint256.Int // output for this crossing
	feeAmount       uint256.Int // amountIn * feePips / (1e6 - feePips)
	totalCost       uint256.Int // amountIn + feeAmount (for quick comparison)
}

// V3Pool is a pre-loaded Uniswap V3 / Pharaoh V3 pool.
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

	// Pre-computed swap steps for empty word boundaries.
	// Key: boundary tick. For each empty word crossing between initialized ticks,
	// we store the exact computeSwapStep result. Lossless.
	preStepsDown map[int32]*v3PrecomputedStep // zeroForOne direction
	preStepsUp   map[int32]*v3PrecomputedStep // oneForZero direction
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

	// Read dynamic fee from storage if the layout supports it (e.g. RamsesV3/PharaohV2).
	// This overrides the hardcoded registry value which may be stale due to setFee().
	if layout.hasFeeSlot {
		data, feeErr := bytesReader(poolAddr, layout.feeSlot)
		if feeErr == nil {
			var feeVal uint256.Int
			feeVal.SetBytes32(data[:])
			storageFee := uint32(feeVal.Uint64() & 0xFFFFFF)
			if storageFee > 0 {
				fee = storageFee
			}
		}
	}

	sqrtPriceX96, tick, err := v3ReadSlot0Bytes(bytesReader, poolAddr, layout.slot0)
	if err != nil {
		return nil
	}

	liquidity, err := v3ReadLiquidityBytes(bytesReader, poolAddr, layout.liquidity)
	if err != nil {
		return nil
	}

	// Pre-load bitmap words centered on current tick (±200 words from current position)
	compressed := tick / tickSpacing
	if tick < 0 && tick%tickSpacing != 0 {
		compressed-- // round towards negative infinity
	}
	centerWord := int16(compressed >> 8)
	bitmapWords := make(map[int16]uint256.Int)
	for wordPos := centerWord - 200; wordPos <= centerWord+200; wordPos++ {
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

	pool := &V3Pool{
		addr:             addr,
		fee:              fee,
		tickSpacing:      tickSpacing,
		sqrtPriceX96:     sqrtPriceX96,
		tick:             tick,
		liquidity:        liquidity,
		bitmapWords:      bitmapWords,
		tickLiquidityNet: tickLiquidityNet,
		preStepsDown:     make(map[int32]*v3PrecomputedStep),
		preStepsUp:       make(map[int32]*v3PrecomputedStep),
	}

	// Pre-compute swap steps for empty word boundaries.
	// Walk from current tick outward in both directions, computing what
	// computeSwapStepU256 would return for each empty word crossing.
	pool.precomputeSteps()

	return pool
}

func (p *V3Pool) precomputeSteps() {
	feePips := uint256.NewInt(uint64(p.fee))
	feeComplement := uint256.NewInt(1_000_000 - uint64(p.fee))

	for _, zeroForOne := range []bool{true, false} {
		currentLiquidity := new(uint256.Int).Set(&p.liquidity)
		currentTick := p.tick
		// Track sqrtPrice at current position for computing amountIn/Out
		currentSqrtPrice := new(uint256.Int).Set(&p.sqrtPriceX96)

		for step := 0; step < 500; step++ {
			compressed := v3FloorDiv(int(currentTick), int(p.tickSpacing))
			var nextTick int32
			var initialized bool

			if zeroForOne {
				wordPos, bitPos := v3Position(compressed)
				word := p.bitmapWords[wordPos]
				var mask, masked uint256.Int
				mask.Lsh(uint256.NewInt(1), uint(bitPos)+1)
				mask.SubUint64(&mask, 1)
				masked.And(&word, &mask)
				if !masked.IsZero() {
					msb := masked.BitLen() - 1
					nextTick = int32((compressed - (int(bitPos) - msb)) * int(p.tickSpacing))
					initialized = true
				} else {
					nextTick = int32((compressed - int(bitPos)) * int(p.tickSpacing))
				}
			} else {
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
									goto foundPreLsb
								}
							}
						}
					}
				foundPreLsb:
					nextTick = int32((compressed + (lsb - int(bitPos))) * int(p.tickSpacing))
					initialized = true
				} else {
					nextTick = int32((compressed + (255 - int(bitPos))) * int(p.tickSpacing))
				}
			}

			if nextTick < algebraMinTick {
				nextTick = algebraMinTick
			}
			if nextTick > algebraMaxTick {
				nextTick = algebraMaxTick
			}

			sqrtPriceNext := getSqrtRatioAtTickU256(nextTick)

			if !initialized && !currentLiquidity.IsZero() {
				// Pre-compute the full crossing for this empty word boundary.
				// This is exactly what computeSwapStepU256 computes when isMax=true.
				var amountIn, amountOut uint256.Int
				if zeroForOne {
					amountIn = sGetAmount0DeltaU256(&sqrtPriceNext, currentSqrtPrice, currentLiquidity, true)
					amountOut = sGetAmount1DeltaU256(&sqrtPriceNext, currentSqrtPrice, currentLiquidity, false)
				} else {
					amountIn = sGetAmount1DeltaU256(currentSqrtPrice, &sqrtPriceNext, currentLiquidity, true)
					amountOut = sGetAmount0DeltaU256(currentSqrtPrice, &sqrtPriceNext, currentLiquidity, false)
				}

				var feeAmount uint256.Int
				feeAmount = mulDivRoundingUpU256(&amountIn, feePips, feeComplement)

				var totalCost uint256.Int
				totalCost.Add(&amountIn, &feeAmount)

				ps := &v3PrecomputedStep{
					sqrtPriceTarget: sqrtPriceNext,
					amountIn:        amountIn,
					amountOut:       amountOut,
					feeAmount:       feeAmount,
					totalCost:       totalCost,
				}

				if zeroForOne {
					p.preStepsDown[nextTick] = ps
				} else {
					p.preStepsUp[nextTick] = ps
				}
			}

			// Move to next position
			currentSqrtPrice.Set(&sqrtPriceNext)
			if initialized {
				liqNet := p.tickLiquidityNet[nextTick]
				if zeroForOne {
					currentLiquidity.Sub(currentLiquidity, &liqNet)
					currentTick = nextTick - 1
				} else {
					currentLiquidity.Add(currentLiquidity, &liqNet)
					currentTick = nextTick
				}
			} else {
				if zeroForOne {
					currentTick = nextTick - 1
				} else {
					currentTick = nextTick
				}
			}
		}
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
		nextTick, initialized := p.nextInitializedTick(tick, zeroForOne)

		if nextTick < algebraMinTick {
			nextTick = algebraMinTick
		}
		if nextTick > algebraMaxTick {
			nextTick = algebraMaxTick
		}

		// Check for pre-computed step (empty word, full crossing)
		if !initialized {
			var preSteps map[int32]*v3PrecomputedStep
			if zeroForOne {
				preSteps = p.preStepsDown
			} else {
				preSteps = p.preStepsUp
			}
			if ps, ok := preSteps[nextTick]; ok && !amountRemaining.Lt(&ps.totalCost) {
				// Full crossing — use pre-computed values (lossless)
				amountRemaining.Sub(&amountRemaining, &ps.amountIn)
				amountRemaining.Sub(&amountRemaining, &ps.feeAmount)
				amountOut.Add(&amountOut, &ps.amountOut)
				sqrtPriceX96.Set(&ps.sqrtPriceTarget)
				if zeroForOne {
					tick = nextTick - 1
				} else {
					tick = nextTick
				}
				continue
			}
		}

		// Standard path: compute swap step
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

// nextInitializedTick scans ONE pre-loaded bitmap word (exact same as Solidity).
func (p *V3Pool) nextInitializedTick(tick int32, zeroForOne bool) (int32, bool) {
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
