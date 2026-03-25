package formulas

import (
	"math/big"
	"sort"
	"strings"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// v3TickData holds a single initialized tick's position and liquidityNet.
type v3TickData struct {
	tick         int32
	liquidityNet uint256.Int // signed, stored as two's complement uint256
}

// V3Pool is a pre-loaded Uniswap V3 / Pharaoh V3 pool.
// Construction reads slot0, liquidity, scans all bitmap words, and reads
// liquidityNet for each initialized tick. Quote is pure math with binary search.
type V3Pool struct {
	addr         common.Address
	fee          uint32
	tickSpacing  int32
	sqrtPriceX96 uint256.Int
	tick         int32
	liquidity    uint256.Int
	ticks        []v3TickData // sorted by tick index
}

func newV3Pool(addr common.Address, reader StorageReader) *V3Pool {
	poolAddress := strings.ToLower(addr.Hex())

	feeInfo, ok := v3PoolFees[poolAddress]
	if !ok {
		return nil
	}
	fee := uint32(feeInfo[0])
	tickSpacing := feeInfo[1]

	// Create readers for layout resolution and bytes-based reads
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

	// Scan all bitmap words to find initialized tick positions.
	// Range: +-200 words covers the full practical tick range.
	var ticks []v3TickData
	for wordPos := int16(-200); wordPos <= 200; wordPos++ {
		word, err := v3ReadBitmapWordBytes(bytesReader, poolAddr, layout.bitmap, wordPos)
		if err != nil {
			continue
		}
		if word.IsZero() {
			continue
		}
		// Extract each set bit
		for bit := 0; bit < 256; bit++ {
			if word[bit/64]&(1<<uint(bit%64)) != 0 {
				compressed := int(wordPos)*256 + bit
				tickIdx := int32(compressed) * tickSpacing
				// Read liquidityNet for this tick
				liqNet, err := v3ReadTickLiquidityNetBytes(bytesReader, poolAddr, layout.ticks, tickIdx)
				if err != nil {
					continue
				}
				ticks = append(ticks, v3TickData{
					tick:         tickIdx,
					liquidityNet: liqNet,
				})
			}
		}
	}

	// Sort ticks by tick index
	sort.Slice(ticks, func(i, j int) bool {
		return ticks[i].tick < ticks[j].tick
	})

	return &V3Pool{
		addr:         addr,
		fee:          fee,
		tickSpacing:  tickSpacing,
		sqrtPriceX96: sqrtPriceX96,
		tick:         tick,
		liquidity:    liquidity,
		ticks:        ticks,
	}
}

func (p *V3Pool) Address() common.Address {
	return p.addr
}

// TickCount returns the number of pre-loaded initialized ticks.
func (p *V3Pool) TickCount() int {
	return len(p.ticks)
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
				liquidityNet := p.getTickLiquidityNet(nextTick)
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

// nextInitializedTick finds the next initialized tick using binary search on the pre-loaded ticks array.
// This replaces bitmap scanning with a simple array search.
func (p *V3Pool) nextInitializedTick(tick int32, zeroForOne bool) (int32, bool) {
	if len(p.ticks) == 0 {
		if zeroForOne {
			return algebraMinTick, false
		}
		return algebraMaxTick, false
	}

	if zeroForOne {
		// Find largest initialized tick <= tick
		target := tick
		idx := sort.Search(len(p.ticks), func(i int) bool {
			return p.ticks[i].tick > target
		})
		idx--
		if idx >= 0 {
			return p.ticks[idx].tick, true
		}
		// No initialized tick below — jump straight to min
		return algebraMinTick, false
	}

	// Find smallest initialized tick > tick
	idx := sort.Search(len(p.ticks), func(i int) bool {
		return p.ticks[i].tick > tick
	})
	if idx < len(p.ticks) {
		return p.ticks[idx].tick, true
	}
	// No initialized tick above — jump straight to max
	return algebraMaxTick, false
}

// getTickLiquidityNet returns the pre-loaded liquidityNet for a tick.
func (p *V3Pool) getTickLiquidityNet(tickIdx int32) uint256.Int {
	idx := sort.Search(len(p.ticks), func(i int) bool {
		return p.ticks[i].tick >= tickIdx
	})
	if idx < len(p.ticks) && p.ticks[idx].tick == tickIdx {
		return p.ticks[idx].liquidityNet
	}
	return uint256.Int{}
}
