package formulas

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
)

// Uniswap V4 concentrated liquidity formula.
//
// V4 uses a singleton PoolManager that holds all pool state. The math is identical
// to V3 (sqrtPriceX96, ticks, liquidity), but state is read via extsload on the
// PoolManager contract instead of individual pool contracts.
//
// Storage layout (from Uniswap's StateLibrary.sol):
//   POOLS_SLOT = 6  (mapping(PoolId => Pool.State) in PoolManager)
//   Pool.State offsets:
//     0: slot0 (sqrtPriceX96:160 | tick:24 | protocolFee:24 | lpFee:24)
//     1: feeGrowthGlobal0X128
//     2: feeGrowthGlobal1X128
//     3: liquidity (uint128)
//     4: ticks mapping (mapping(int24 => TickInfo))
//     5: tickBitmap mapping (mapping(int16 => uint256))

const (
	V4PoolManagerAddress = "0x06380C0e0912312B5150364B9DC4542BA0DbBc85"

	// Storage slot of _pools mapping in PoolManager
	v4PoolsSlot = 6

	// Offsets within Pool.State struct
	v4Slot0Offset      = 0
	v4LiquidityOffset  = 3
	v4TicksOffset      = 4
	v4TickBitmapOffset = 5

	// Fee denominator: 1e6 = 100%
	v4MaxSwapFee = 1000000
)

// ArenaHook address and fee helper
const (
	arenaHookAddress   = "0xe32a5d788c568fc5a671255d17b618e70552e044"
	arenaFeeHelperAddr = "0x537505da49b4249b576fc8d00028bfddf6189077"
	arenaHookMaxFee    = 1_000_000 // ppm denominator
)

// V4State holds the pool state needed for swap computation.
type V4State struct {
	SqrtPriceX96 *big.Int
	Tick         int32
	ProtocolFee  uint32
	LpFee        uint32
	Liquidity    *big.Int // uint128
	TickSpacing  int32
	PoolId       [32]byte
	HookFeePpm   uint32 // ArenaHook fee in ppm (0 if no hook)
}

// ParseV4ExtraData extracts pool parameters from the extraData string.
// Format: id=0x...,fee=...,ts=...,hooks=0x...
func ParseV4ExtraData(extraData string) (poolId [32]byte, fee uint32, tickSpacing int32, hooks common.Address, err error) {
	kv := make(map[string]string)
	for _, part := range strings.Split(extraData, ",") {
		eq := strings.Index(part, "=")
		if eq > 0 {
			kv[part[:eq]] = part[eq+1:]
		}
	}

	idHex := kv["id"]
	if idHex == "" {
		err = fmt.Errorf("missing id in extraData")
		return
	}
	idBytes := common.FromHex(idHex)
	if len(idBytes) != 32 {
		err = fmt.Errorf("invalid pool id length: %d", len(idBytes))
		return
	}
	copy(poolId[:], idBytes)

	if feeStr, ok := kv["fee"]; ok {
		f := new(big.Int)
		f.SetString(feeStr, 10)
		fee = uint32(f.Uint64())
	}

	if tsStr, ok := kv["ts"]; ok {
		t := new(big.Int)
		t.SetString(tsStr, 10)
		tickSpacing = int32(t.Int64())
	}

	if hooksStr, ok := kv["hooks"]; ok {
		hooks = common.HexToAddress(hooksStr)
	}

	return
}

// v4GetPoolStateSlot computes keccak256(abi.encodePacked(poolId, POOLS_SLOT))
func v4GetPoolStateSlot(poolId [32]byte) common.Hash {
	slotPadded := common.LeftPadBytes(big.NewInt(int64(v4PoolsSlot)).Bytes(), 32)
	data := make([]byte, 64)
	copy(data[0:32], poolId[:])
	copy(data[32:64], slotPadded)
	return crypto.Keccak256Hash(data)
}

// v4GetTickBitmapSlot computes the storage slot for tickBitmap[wordPos]
func v4GetTickBitmapSlot(stateSlot common.Hash, wordPos int16) common.Hash {
	tickBitmapMapping := new(big.Int).Add(stateSlot.Big(), big.NewInt(int64(v4TickBitmapOffset)))

	// int256(wordPos) in two's complement
	wordPosBig := big.NewInt(int64(wordPos))
	if wordPos < 0 {
		wordPosBig.Add(new(big.Int).Lsh(big.NewInt(1), 256), wordPosBig)
	}
	wordPosPadded := common.LeftPadBytes(wordPosBig.Bytes(), 32)
	mappingPadded := common.LeftPadBytes(tickBitmapMapping.Bytes(), 32)

	packed := make([]byte, 64)
	copy(packed[0:32], wordPosPadded)
	copy(packed[32:64], mappingPadded)
	return crypto.Keccak256Hash(packed)
}

// v4GetTickInfoSlot computes the storage slot for ticks[tick]
func v4GetTickInfoSlot(stateSlot common.Hash, tick int32) common.Hash {
	ticksMapping := new(big.Int).Add(stateSlot.Big(), big.NewInt(int64(v4TicksOffset)))

	// int256(tick) in two's complement
	tickBig := big.NewInt(int64(tick))
	if tick < 0 {
		tickBig.Add(new(big.Int).Lsh(big.NewInt(1), 256), tickBig)
	}
	tickPadded := common.LeftPadBytes(tickBig.Bytes(), 32)
	mappingPadded := common.LeftPadBytes(ticksMapping.Bytes(), 32)

	packed := make([]byte, 64)
	copy(packed[0:32], tickPadded)
	copy(packed[32:64], mappingPadded)
	return crypto.Keccak256Hash(packed)
}

// v4ReadSlot reads a single storage slot from the PoolManager via StateReader.
func v4ReadSlot(reader StateReader, slot common.Hash) (common.Hash, error) {
	raw, err := reader(V4PoolManagerAddress, slot.Big())
	if err != nil {
		return common.Hash{}, err
	}
	return common.BytesToHash(raw[:]), nil
}

// v4ReadSlotRange reads multiple consecutive storage slots from the PoolManager via StateReader.
func v4ReadSlotRange(reader StateReader, startSlot common.Hash, nSlots int) ([]common.Hash, error) {
	results := make([]common.Hash, nSlots)
	slotNum := startSlot.Big()
	for i := 0; i < nSlots; i++ {
		raw, err := reader(V4PoolManagerAddress, slotNum)
		if err != nil {
			return nil, err
		}
		results[i] = common.BytesToHash(raw[:])
		slotNum = new(big.Int).Add(slotNum, big.NewInt(1))
	}
	return results, nil
}

// FetchV4State reads the pool state from the PoolManager via direct storage reads.
// hookFeePpm must be provided externally (e.g. from the pool registry) since we
// can't make eth_call from a pure StateReader. Pass 0 for pools without hooks.
func FetchV4State(reader StateReader, extraData string, hookFeePpm uint32) (*V4State, error) {
	poolId, _, tickSpacing, _, err := ParseV4ExtraData(extraData)
	if err != nil {
		return nil, fmt.Errorf("parse extraData: %w", err)
	}

	stateSlot := v4GetPoolStateSlot(poolId)

	// Read 4 consecutive slots: slot0, feeGrowthGlobal0, feeGrowthGlobal1, liquidity
	slots, err := v4ReadSlotRange(reader, stateSlot, 4)
	if err != nil {
		return nil, fmt.Errorf("storage read: %w", err)
	}

	// Parse slot0: 160 bits sqrtPriceX96 | 24 bits tick | 24 bits protocolFee | 24 bits lpFee
	slot0Data := slots[0].Big()
	mask160 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 160), big.NewInt(1))
	sqrtPriceX96 := new(big.Int).And(slot0Data, mask160)

	// tick: bits [160:184], sign-extended from 24 bits
	tickRaw := new(big.Int).Rsh(slot0Data, 160)
	tickRaw.And(tickRaw, big.NewInt(0xFFFFFF))
	tick := v4SignExtend24(tickRaw)

	// protocolFee: bits [184:208]
	protocolFee := new(big.Int).Rsh(slot0Data, 184)
	protocolFee.And(protocolFee, big.NewInt(0xFFFFFF))

	// lpFee: bits [208:232]
	lpFee := new(big.Int).Rsh(slot0Data, 208)
	lpFee.And(lpFee, big.NewInt(0xFFFFFF))

	// liquidity at offset 3 (uint128)
	mask128 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	liquidity := new(big.Int).And(slots[3].Big(), mask128)

	return &V4State{
		SqrtPriceX96: sqrtPriceX96,
		Tick:         tick,
		ProtocolFee:  uint32(protocolFee.Uint64()),
		LpFee:        uint32(lpFee.Uint64()),
		Liquidity:    liquidity,
		TickSpacing:  tickSpacing,
		PoolId:       poolId,
		HookFeePpm:   hookFeePpm,
	}, nil
}

// FetchV4StateFromBytes reads V4 pool state using BytesStateReader (zero big.Int overhead).
func FetchV4StateFromBytes(reader BytesStateReader, poolId [32]byte, tickSpacing int32, hookFeePpm uint32) (*V4State, error) {
	stateSlot := v4GetPoolStateSlot(poolId)
	pmAddr := v4PoolManagerAddr

	// Read slot0
	slot0Key := bigIntTo32(stateSlot.Big())
	slot0Data, err := reader(pmAddr, slot0Key)
	if err != nil {
		return nil, fmt.Errorf("read slot0: %w", err)
	}

	// Read liquidity (offset 3)
	liqSlot := bigIntTo32(new(big.Int).Add(stateSlot.Big(), big.NewInt(int64(v4LiquidityOffset))))
	liqData, err := reader(pmAddr, liqSlot)
	if err != nil {
		return nil, fmt.Errorf("read liquidity: %w", err)
	}

	// Parse slot0: sqrtPriceX96 is lower 160 bits, tick at [160:184], protocolFee at [184:208], lpFee at [208:232]
	slot0Big := new(big.Int).SetBytes(slot0Data[:])
	mask160 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 160), big.NewInt(1))
	sqrtPriceX96 := new(big.Int).And(slot0Big, mask160)

	tickRaw := new(big.Int).Rsh(slot0Big, 160)
	tickRaw.And(tickRaw, big.NewInt(0xFFFFFF))
	tick := v4SignExtend24(tickRaw)

	protocolFee := new(big.Int).Rsh(slot0Big, 184)
	protocolFee.And(protocolFee, big.NewInt(0xFFFFFF))

	lpFee := new(big.Int).Rsh(slot0Big, 208)
	lpFee.And(lpFee, big.NewInt(0xFFFFFF))

	mask128 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	liquidity := new(big.Int).And(new(big.Int).SetBytes(liqData[:]), mask128)

	return &V4State{
		SqrtPriceX96: sqrtPriceX96,
		Tick:         tick,
		ProtocolFee:  uint32(protocolFee.Uint64()),
		LpFee:        uint32(lpFee.Uint64()),
		Liquidity:    liquidity,
		TickSpacing:  tickSpacing,
		PoolId:       poolId,
		HookFeePpm:   hookFeePpm,
	}, nil
}

// v4SignExtend24 sign-extends a 24-bit value to int32.
func v4SignExtend24(v *big.Int) int32 {
	val := int32(v.Int64() & 0xFFFFFF)
	if val >= 0x800000 {
		val -= 0x1000000
	}
	return val
}

// v4NextInitializedTickWithinOneWord reads the tick bitmap and returns the next
// initialized tick within one word. 1 storage read.
func v4NextInitializedTickWithinOneWord(
	reader StateReader, stateSlot common.Hash, tick int32, tickSpacing int32, lte bool,
) (int32, bool, error) {
	compressed := v4Compress(tick, tickSpacing)

	if lte {
		wordPos, bitPos := v4Position(compressed)
		// All the 1s at or to the right of bitPos
		mask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(bitPos)+1), big.NewInt(1))

		bitmapSlot := v4GetTickBitmapSlot(stateSlot, wordPos)
		bitmapData, err := v4ReadSlot(reader, bitmapSlot)
		if err != nil {
			return 0, false, err
		}
		word := bitmapData.Big()
		masked := new(big.Int).And(word, mask)
		initialized := masked.Sign() != 0

		if initialized {
			msb := v4MostSignificantBit(masked)
			next := (compressed - int32(uint32(bitPos)-uint32(msb))) * tickSpacing
			return next, true, nil
		}
		next := (compressed - int32(bitPos)) * tickSpacing
		return next, false, nil
	}

	// Search to the right
	compressed++
	wordPos, bitPos := v4Position(compressed)
	// All the 1s at or to the left of bitPos
	mask := new(big.Int).Not(new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(bitPos)), big.NewInt(1)))
	mask.And(mask, maxUint256)

	bitmapSlot := v4GetTickBitmapSlot(stateSlot, wordPos)
	bitmapData, err := v4ReadSlot(reader, bitmapSlot)
	if err != nil {
		return 0, false, err
	}
	word := bitmapData.Big()
	masked := new(big.Int).And(word, mask)
	initialized := masked.Sign() != 0

	if initialized {
		lsb := v4LeastSignificantBit(masked)
		next := (compressed + int32(uint32(lsb)-uint32(bitPos))) * tickSpacing
		return next, true, nil
	}
	next := (compressed + int32(uint32(255)-uint32(bitPos))) * tickSpacing
	return next, false, nil
}

// v4Compress rounds tick towards negative infinity by tickSpacing.
func v4Compress(tick int32, tickSpacing int32) int32 {
	compressed := tick / tickSpacing
	if tick < 0 && tick%tickSpacing != 0 {
		compressed--
	}
	return compressed
}

// v4Position returns (wordPos, bitPos) for a compressed tick.
func v4Position(compressed int32) (int16, uint8) {
	return int16(compressed >> 8), uint8(compressed & 0xFF)
}

// v4GetTickLiquidityNet reads the liquidityNet from a tick's TickInfo struct.
// TickInfo first slot: liquidityGross (lower 128 bits) | liquidityNet (upper 128 bits).
// 1 storage read.
func v4GetTickLiquidityNet(reader StateReader, stateSlot common.Hash, tick int32) (*big.Int, error) {
	tickSlot := v4GetTickInfoSlot(stateSlot, tick)
	data, err := v4ReadSlot(reader, tickSlot)
	if err != nil {
		return nil, err
	}

	// liquidityNet is in the upper 128 bits
	val := data.Big()
	liquidityNet := new(big.Int).Rsh(val, 128)
	// Sign-extend from 128 bits
	if liquidityNet.Bit(127) == 1 {
		liquidityNet.Sub(liquidityNet, new(big.Int).Lsh(big.NewInt(1), 128))
	}
	return liquidityNet, nil
}

// v4MostSignificantBit returns the MSB of a uint256.
func v4MostSignificantBit(x *big.Int) uint8 {
	if x.Sign() == 0 {
		return 0
	}
	return uint8(x.BitLen() - 1)
}

// v4LeastSignificantBit returns the LSB of a uint256.
func v4LeastSignificantBit(x *big.Int) uint8 {
	if x.Sign() == 0 {
		return 0
	}
	neg := new(big.Int).Neg(x)
	isolated := new(big.Int).And(x, neg)
	return uint8(isolated.BitLen() - 1)
}

// QuoteV4 computes the output amount for a V4 concentrated liquidity swap.
// This is an exact-input swap. Reads tick bitmap and tick data on-the-fly via storage reads.
// zeroForOne: true if swapping token0 -> token1.
func QuoteV4(reader StateReader, state *V4State, amountIn *big.Int, zeroForOne bool) (*big.Int, error) {
	if amountIn.Sign() == 0 {
		return big.NewInt(0), nil
	}

	// Determine swap fee (same logic as V4 Pool.swap)
	var protocolFee uint32
	if zeroForOne {
		protocolFee = state.ProtocolFee & 0xFFF // lower 12 bits: 0->1 fee
	} else {
		protocolFee = (state.ProtocolFee >> 12) & 0xFFF // upper 12 bits: 1->0 fee
	}

	var swapFee uint32
	if protocolFee == 0 {
		swapFee = state.LpFee
	} else {
		// calculateSwapFee: protocolFee + lpFee - (protocolFee * lpFee / 1_000_000)
		swapFee = protocolFee + state.LpFee - (protocolFee*state.LpFee)/uint32(v4MaxSwapFee)
	}

	// Price limit
	var sqrtPriceLimitX96 *big.Int
	if zeroForOne {
		sqrtPriceLimitX96 = new(big.Int).Add(algebraMinSqrtRatio, big.NewInt(1))
	} else {
		sqrtPriceLimitX96 = new(big.Int).Sub(algebraMaxSqrtRatio, big.NewInt(1))
	}

	// Swap state
	sqrtPriceX96 := new(big.Int).Set(state.SqrtPriceX96)
	tick := state.Tick
	liquidity := new(big.Int).Set(state.Liquidity)

	// amountSpecifiedRemaining is negative for exact input (V4 convention)
	amountSpecifiedRemaining := new(big.Int).Neg(amountIn)
	amountCalculated := big.NewInt(0)

	stateSlot := v4GetPoolStateSlot(state.PoolId)

	// Swap loop - walk through ticks
	for amountSpecifiedRemaining.Sign() != 0 && sqrtPriceX96.Cmp(sqrtPriceLimitX96) != 0 {
		// Find next initialized tick within one word (1 storage read)
		tickNext, initialized, err := v4NextInitializedTickWithinOneWord(
			reader, stateSlot, tick, state.TickSpacing, zeroForOne,
		)
		if err != nil {
			return nil, fmt.Errorf("nextInitializedTick: %w", err)
		}

		// Clamp
		if tickNext <= algebraMinTick {
			tickNext = algebraMinTick
		}
		if tickNext >= algebraMaxTick {
			tickNext = algebraMaxTick
		}

		sqrtPriceNextX96 := getSqrtRatioAtTick(tickNext)

		// Target price
		sqrtPriceTargetX96 := v4GetSqrtPriceTarget(zeroForOne, sqrtPriceNextX96, sqrtPriceLimitX96)

		// Compute swap step (reuses V3 math)
		newSqrtPriceX96, stepAmountIn, stepAmountOut, stepFeeAmount := v4ComputeSwapStep(
			sqrtPriceX96, sqrtPriceTargetX96, liquidity, amountSpecifiedRemaining, swapFee,
		)
		sqrtPriceX96 = newSqrtPriceX96

		// Exact input: amountSpecified < 0
		amountSpecifiedRemaining.Add(amountSpecifiedRemaining, new(big.Int).Add(stepAmountIn, stepFeeAmount))
		amountCalculated.Add(amountCalculated, stepAmountOut)

		// Cross tick if we reached the next tick price
		if sqrtPriceX96.Cmp(sqrtPriceNextX96) == 0 {
			if initialized {
				liquidityNet, err := v4GetTickLiquidityNet(reader, stateSlot, tickNext)
				if err != nil {
					return nil, fmt.Errorf("getTickLiquidityNet: %w", err)
				}
				if zeroForOne {
					liquidityNet.Neg(liquidityNet)
				}
				liquidity.Add(liquidity, liquidityNet)
			}
			if zeroForOne {
				tick = tickNext - 1
			} else {
				tick = tickNext
			}
		} else if sqrtPriceX96.Cmp(state.SqrtPriceX96) != 0 {
			tick = getTickAtSqrtRatio(sqrtPriceX96)
		}
	}

	// Apply ArenaHook fee if present: feeAmount = floor(output * hookFeePpm / 1_000_000)
	if state.HookFeePpm > 0 && amountCalculated.Sign() > 0 {
		feeAmt := mulDiv(amountCalculated, new(big.Int).SetUint64(uint64(state.HookFeePpm)), big.NewInt(arenaHookMaxFee))
		amountCalculated.Sub(amountCalculated, feeAmt)
	}

	return amountCalculated, nil
}

// v4GetSqrtPriceTarget returns the target price for the next swap step.
func v4GetSqrtPriceTarget(zeroForOne bool, sqrtPriceNextX96, sqrtPriceLimitX96 *big.Int) *big.Int {
	if zeroForOne {
		if sqrtPriceNextX96.Cmp(sqrtPriceLimitX96) < 0 {
			return new(big.Int).Set(sqrtPriceLimitX96)
		}
		return new(big.Int).Set(sqrtPriceNextX96)
	}
	if sqrtPriceNextX96.Cmp(sqrtPriceLimitX96) < 0 {
		return new(big.Int).Set(sqrtPriceNextX96)
	}
	return new(big.Int).Set(sqrtPriceLimitX96)
}

// v4ComputeSwapStep implements SwapMath.computeSwapStep from V4.
// amountRemaining is negative for exact input.
// feePips is in pips (1e6 = 100%).
func v4ComputeSwapStep(
	sqrtPriceCurrentX96, sqrtPriceTargetX96 *big.Int,
	liquidity *big.Int,
	amountRemaining *big.Int,
	feePips uint32,
) (sqrtPriceNextX96, amountIn, amountOut, feeAmount *big.Int) {
	zeroForOne := sqrtPriceCurrentX96.Cmp(sqrtPriceTargetX96) >= 0
	exactIn := amountRemaining.Sign() < 0

	feePipsBig := new(big.Int).SetUint64(uint64(feePips))
	maxFeeBig := big.NewInt(int64(v4MaxSwapFee))

	if exactIn {
		absRemaining := new(big.Int).Neg(amountRemaining)
		amountRemainingLessFee := mulDiv(absRemaining, new(big.Int).Sub(maxFeeBig, feePipsBig), maxFeeBig)

		if zeroForOne {
			amountIn = v3GetAmount0Delta(sqrtPriceTargetX96, sqrtPriceCurrentX96, liquidity, true)
		} else {
			amountIn = v3GetAmount1Delta(sqrtPriceCurrentX96, sqrtPriceTargetX96, liquidity, true)
		}

		if amountRemainingLessFee.Cmp(amountIn) >= 0 {
			sqrtPriceNextX96 = new(big.Int).Set(sqrtPriceTargetX96)
			if feePips == uint32(v4MaxSwapFee) {
				feeAmount = new(big.Int).Set(amountIn)
			} else {
				feeAmount = mulDivRoundingUp(amountIn, feePipsBig, new(big.Int).Sub(maxFeeBig, feePipsBig))
			}
		} else {
			amountIn = new(big.Int).Set(amountRemainingLessFee)
			sqrtPriceNextX96 = v3GetNextSqrtPriceFromInput(sqrtPriceCurrentX96, liquidity, amountRemainingLessFee, zeroForOne)
			feeAmount = new(big.Int).Sub(absRemaining, amountIn)
		}

		if zeroForOne {
			amountOut = v3GetAmount1Delta(sqrtPriceNextX96, sqrtPriceCurrentX96, liquidity, false)
		} else {
			amountOut = v3GetAmount0Delta(sqrtPriceCurrentX96, sqrtPriceNextX96, liquidity, false)
		}
	} else {
		// Exact output (not used in our harness, but included for completeness)
		if zeroForOne {
			amountOut = v3GetAmount1Delta(sqrtPriceTargetX96, sqrtPriceCurrentX96, liquidity, false)
		} else {
			amountOut = v3GetAmount0Delta(sqrtPriceCurrentX96, sqrtPriceTargetX96, liquidity, false)
		}

		if new(big.Int).Set(amountRemaining).Cmp(amountOut) >= 0 {
			sqrtPriceNextX96 = new(big.Int).Set(sqrtPriceTargetX96)
		} else {
			amountOut = new(big.Int).Set(amountRemaining)
			sqrtPriceNextX96 = v3GetNextSqrtPriceFromOutput(sqrtPriceCurrentX96, liquidity, amountOut, zeroForOne)
		}

		if zeroForOne {
			amountIn = v3GetAmount0Delta(sqrtPriceNextX96, sqrtPriceCurrentX96, liquidity, true)
		} else {
			amountIn = v3GetAmount1Delta(sqrtPriceCurrentX96, sqrtPriceNextX96, liquidity, true)
		}

		feeAmount = mulDivRoundingUp(amountIn, feePipsBig, new(big.Int).Sub(maxFeeBig, feePipsBig))
	}

	return
}

// v3GetNextSqrtPriceFromOutput computes the next sqrt price given an output amount.
func v3GetNextSqrtPriceFromOutput(sqrtPX96, liquidity, amountOut *big.Int, zeroForOne bool) *big.Int {
	if zeroForOne {
		return v3GetNextSqrtPriceFromAmount1RoundingDown(sqrtPX96, liquidity, amountOut, false)
	}
	return v3GetNextSqrtPriceFromAmount0RoundingUp(sqrtPX96, liquidity, amountOut, false)
}
