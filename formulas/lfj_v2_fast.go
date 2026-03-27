package formulas

import (
	"math/big"
	"sync"

	"github.com/ava-labs/libevm/crypto"
	"github.com/holiman/uint256"
)

// lfj_v2_fast.go — uint256 version of the LFJ V2 hot path.
// Replaces math/big with github.com/holiman/uint256 ([4]uint64, stack-allocated)
// to eliminate GC pressure. Keccak slot results are cached per pool.

// ─── uint256 constants ───

var (
	scaleU256      = new(uint256.Int).Lsh(uint256.NewInt(1), 128)           // 2^128
	precisionU256  *uint256.Int // 1e18, set in init()
	maxUint256U256 = new(uint256.Int).Sub(new(uint256.Int).Lsh(uint256.NewInt(1), 256), uint256.NewInt(1))
	bpMaxU256      = uint256.NewInt(10000)
)

func init() {
	precisionU256 = uint256.MustFromDecimal("1000000000000000000")
}

// ─── pow128U256 ───

// pow128U256 computes base^exponent in 128.128 fixed-point using uint256.Int.
// Same algorithm as pow128 in lfj_v2.go but zero heap allocations.
func pow128U256(base *uint256.Int, exponent int32) uint256.Int {
	if exponent == 0 {
		var r uint256.Int
		r.Set(scaleU256)
		return r
	}

	invert := false
	absY := int64(exponent)
	if absY < 0 {
		absY = -absY
		invert = true
	}

	var squared uint256.Int
	squared.Set(base)
	if squared.Gt(mask128U256) {
		// squared = maxUint256 / squared
		squared.Div(maxUint256U256, &squared)
		invert = !invert
	}

	var result uint256.Int
	result.Set(scaleU256)

	// Binary exponentiation — 20 bits
	for bit := 0; bit < 20; bit++ {
		if absY&(1<<bit) != 0 {
			result.Mul(&result, &squared)
			result.Rsh(&result, 128)
		}
		if bit < 19 {
			squared.Mul(&squared, &squared)
			squared.Rsh(&squared, 128)
		}
	}

	if result.IsZero() {
		return result
	}

	if invert {
		result.Div(maxUint256U256, &result)
	}

	return result
}

// ─── Price cache using uint256.Int ───

var binPriceCacheU256 sync.Map // map[[2]uint32]uint256.Int

func getPriceFromIdU256(id uint32, binStep uint16) uint256.Int {
	key := [2]uint32{uint32(binStep), id}
	if cached, ok := binPriceCacheU256.Load(key); ok {
		v := cached.(uint256.Int)
		return v
	}

	// base = 2^128 + (binStep * 2^128) / 10000
	var bsScaled, bsDiv, base uint256.Int
	bsScaled.SetUint64(uint64(binStep))
	bsScaled.Lsh(&bsScaled, 128)
	bsDiv.Div(&bsScaled, bpMaxU256)
	base.Add(scaleU256, &bsDiv)

	exponent := int32(id) - int32(realIDShift)
	result := pow128U256(&base, exponent)
	binPriceCacheU256.Store(key, result) // store by value
	return result
}

// ─── Fee calculation (uint64 arithmetic where possible) ───

// getTotalFeeU256 returns the total fee as uint256.Int.
// For most pools the fee fits in uint64, but we use uint256 for consistency.
func getTotalFeeU256(baseFactor, binStep uint16, volAcc uint32, variableFeeControl uint32) uint256.Int {
	bf := uint64(baseFactor) * uint64(binStep) * 1e10
	var total uint256.Int
	total.SetUint64(bf)

	if variableFeeControl != 0 {
		prod := uint64(volAcc) * uint64(binStep)
		prodSq := prod * prod // fits in uint64 if volAcc*binStep < ~4.3e9 (always true for uint20*uint16)
		// variableFee = ceil(prodSq * variableFeeControl / 100)
		vf := prodSq * uint64(variableFeeControl)
		vf = (vf + 99) / 100
		var vfU256 uint256.Int
		vfU256.SetUint64(vf)
		total.Add(&total, &vfU256)
	}

	// Cap to max uint128
	if total.Gt(mask128U256) {
		total.Set(mask128U256)
	}
	return total
}

// getFeeAmountU256 computes: ceil(amount * totalFee / (precision - totalFee))
func getFeeAmountU256(amount, totalFee *uint256.Int) uint256.Int {
	var denom, num, denomMinus1, result uint256.Int
	denom.Sub(precisionU256, totalFee)
	num.Mul(amount, totalFee)
	denomMinus1.SubUint64(&denom, 1)
	num.Add(&num, &denomMinus1)
	result.Div(&num, &denom)
	return result
}

// getFeeAmountFromU256 computes: ceil(amountWithFees * totalFee / precision)
func getFeeAmountFromU256(amountWithFees, totalFee *uint256.Int) uint256.Int {
	var num, precMinus1, result uint256.Int
	num.Mul(amountWithFees, totalFee)
	precMinus1.SubUint64(precisionU256, 1)
	num.Add(&num, &precMinus1)
	result.Div(&num, precisionU256)
	return result
}

// ─── mulShift / shiftDiv using uint256 ───

// mulShiftU256: (x * y) >> offset, optionally round up.
// Uses MulDivOverflow for full 512-bit intermediate precision, matching the
// Solidity Uint256x128Math library which avoids overflow in the product.
func mulShiftU256(x, y *uint256.Int, offset uint, roundUp bool) uint256.Int {
	var divisor uint256.Int
	divisor.Lsh(uint256.NewInt(1), offset)

	var result uint256.Int
	result.MulDivOverflow(x, y, &divisor)

	if roundUp {
		// Check remainder: (x * y) mod (1 << offset) != 0
		var rem uint256.Int
		rem.MulMod(x, y, &divisor)
		if !rem.IsZero() {
			result.AddUint64(&result, 1)
		}
	}
	return result
}

// shiftDivU256: (x << offset) / denom, optionally round up.
// Uses MulDivOverflow for full 512-bit intermediate precision, matching the
// Solidity Uint256x128Math library which avoids overflow in the shifted value.
func shiftDivU256(x *uint256.Int, offset uint, denom *uint256.Int, roundUp bool) uint256.Int {
	var multiplier uint256.Int
	multiplier.Lsh(uint256.NewInt(1), offset)

	var result uint256.Int
	result.MulDivOverflow(x, &multiplier, denom)

	if roundUp {
		// Check remainder: (x * (1 << offset)) mod denom != 0
		var rem uint256.Int
		rem.MulMod(x, &multiplier, denom)
		if !rem.IsZero() {
			result.AddUint64(&result, 1)
		}
	}
	return result
}

// ─── Keccak slot cache ───
// Key: [40]byte = 20-byte pool address hash prefix + 4-byte binID/key + 8-byte slot
// Value: *big.Int (needed by StateReader interface)

// keccakMappingSlot computes keccak256(abi.encode(uint256(key), uint256(slot)))
// and caches the result. Returns a *big.Int for the StateReader interface.
func keccakMappingSlot(key uint32, slotNum uint64) *big.Int {
	// Check cache
	cacheKey := uint64(key)<<32 | slotNum
	if v, ok := keccakSlotCache.Load(cacheKey); ok {
		return v.(*big.Int)
	}

	var data [64]byte
	// key as uint256 big-endian in first 32 bytes
	data[28] = byte(key >> 24)
	data[29] = byte(key >> 16)
	data[30] = byte(key >> 8)
	data[31] = byte(key)
	// slot as uint256 big-endian in second 32 bytes
	data[56] = byte(slotNum >> 56)
	data[57] = byte(slotNum >> 48)
	data[58] = byte(slotNum >> 40)
	data[59] = byte(slotNum >> 32)
	data[60] = byte(slotNum >> 24)
	data[61] = byte(slotNum >> 16)
	data[62] = byte(slotNum >> 8)
	data[63] = byte(slotNum)

	hash := crypto.Keccak256(data[:])
	result := new(big.Int).SetBytes(hash)
	keccakSlotCache.Store(cacheKey, result)
	return result
}

var keccakSlotCache sync.Map // map[uint64]*big.Int

// ─── Bin reading with uint256 ───

func lfjV2ReadBinU256(read StateReader, poolAddress string, binsSlot uint64, binID uint32) (reserveX, reserveY uint256.Int, err error) {
	slot := keccakMappingSlot(binID, binsSlot)
	val, rerr := read(poolAddress, slot)
	if rerr != nil {
		err = rerr
		return
	}
	// lower 128 bits = reserveX, upper 128 bits = reserveY
	reserveX.SetBytes16(val[16:32])
	reserveY.SetBytes16(val[0:16])
	return
}

// ─── Tree traversal with uint256 ───

func lfjV2ReadTreeLevelU256(read StateReader, poolAddress string, levelSlot uint64, key uint32) (uint256.Int, error) {
	slot := keccakMappingSlot(key, levelSlot)
	val, err := read(poolAddress, slot)
	if err != nil {
		return uint256.Int{}, err
	}
	var result uint256.Int
	result.SetBytes32(val[:])
	return result, nil
}

// closestBitRightU256 — same as lfjV2ClosestBitRight but with uint256.
func closestBitRightU256(x *uint256.Int, bit uint8) int {
	if bit == 0 {
		return 256
	}
	shift := uint(256 - uint(bit))
	var shifted uint256.Int
	shifted.Lsh(x, shift)
	if shifted.IsZero() {
		return 256
	}
	msb := uint8(shifted.BitLen() - 1)
	return int(msb) - int(shift)
}

// closestBitLeftU256 — same as lfjV2ClosestBitLeft but with uint256.
func closestBitLeftU256(x *uint256.Int, bit uint8) int {
	if bit >= 255 {
		return 256
	}
	shift := uint(bit + 1)
	var shifted uint256.Int
	shifted.Rsh(x, shift)
	if shifted.IsZero() {
		return 256
	}
	lsb := leastSignificantBitU256(&shifted)
	return int(lsb) + int(shift)
}

// mostSignificantBitU256 returns the position of the highest set bit.
func mostSignificantBitU256(x *uint256.Int) uint8 {
	if x.IsZero() {
		return 0
	}
	return uint8(x.BitLen() - 1)
}

// leastSignificantBitU256 returns the position of the lowest set bit.
// Uses direct word access on [4]uint64 for speed.
func leastSignificantBitU256(x *uint256.Int) uint8 {
	if x.IsZero() {
		return 255
	}
	// uint256.Int is [4]uint64 little-endian: [0] is least significant
	for w := 0; w < 4; w++ {
		word := x[w]
		if word != 0 {
			// Count trailing zeros
			for b := uint8(0); b < 64; b++ {
				if word&(1<<b) != 0 {
					return uint8(w)*64 + b
				}
			}
		}
	}
	return 255
}

type lfjV2LayoutFast struct {
	parametersSlot uint64
	binsSlot       uint64
	treeLevel0Slot uint64
	treeLevel1Slot uint64
	treeLevel2Slot uint64
}

var (
	lfjV2LayoutFastA = lfjV2LayoutFast{3, 6, 7, 8, 9}
	lfjV2LayoutFastB = lfjV2LayoutFast{4, 7, 8, 9, 10}
)

// lfjV2FindFirstRightU256 finds the next non-empty bin with a LOWER ID.
func lfjV2FindFirstRightU256(read StateReader, poolAddress string, layout *lfjV2LayoutFast, id uint32) (uint32, error) {
	key2 := id >> 8
	bit := uint8(id & 0xFF)

	if bit != 0 {
		leaves, err := lfjV2ReadTreeLevelU256(read, poolAddress, layout.treeLevel2Slot, key2)
		if err != nil {
			return 0, err
		}
		closestBit := closestBitRightU256(&leaves, bit)
		if closestBit != 256 {
			return key2<<8 | uint32(closestBit), nil
		}
	}

	key1 := key2 >> 8
	bit = uint8(key2 & 0xFF)

	if bit != 0 {
		leaves, err := lfjV2ReadTreeLevelU256(read, poolAddress, layout.treeLevel1Slot, key1)
		if err != nil {
			return 0, err
		}
		closestBit := closestBitRightU256(&leaves, bit)
		if closestBit != 256 {
			newKey2 := key1<<8 | uint32(closestBit)
			leaves2, err := lfjV2ReadTreeLevelU256(read, poolAddress, layout.treeLevel2Slot, newKey2)
			if err != nil {
				return 0, err
			}
			msb := mostSignificantBitU256(&leaves2)
			return newKey2<<8 | uint32(msb), nil
		}
	}

	bit = uint8(key1 & 0xFF)
	if bit != 0 {
		val, err := read(poolAddress, big.NewInt(int64(layout.treeLevel0Slot)))
		if err != nil {
			return 0, err
		}
		var leaves uint256.Int
		leaves.SetBytes32(val[:])
		closestBit := closestBitRightU256(&leaves, bit)
		if closestBit != 256 {
			newKey1 := uint32(closestBit)
			leaves1, err := lfjV2ReadTreeLevelU256(read, poolAddress, layout.treeLevel1Slot, newKey1)
			if err != nil {
				return 0, err
			}
			newKey2 := newKey1<<8 | uint32(mostSignificantBitU256(&leaves1))
			leaves2, err := lfjV2ReadTreeLevelU256(read, poolAddress, layout.treeLevel2Slot, newKey2)
			if err != nil {
				return 0, err
			}
			return newKey2<<8 | uint32(mostSignificantBitU256(&leaves2)), nil
		}
	}

	return 0xFFFFFF, nil
}

// lfjV2FindFirstLeftU256 finds the next non-empty bin with a HIGHER ID.
func lfjV2FindFirstLeftU256(read StateReader, poolAddress string, layout *lfjV2LayoutFast, id uint32) (uint32, error) {
	key2 := id >> 8
	bit := uint8(id & 0xFF)

	if bit != 255 {
		leaves, err := lfjV2ReadTreeLevelU256(read, poolAddress, layout.treeLevel2Slot, key2)
		if err != nil {
			return 0, err
		}
		closestBit := closestBitLeftU256(&leaves, bit)
		if closestBit != 256 {
			return key2<<8 | uint32(closestBit), nil
		}
	}

	key1 := key2 >> 8
	bit = uint8(key2 & 0xFF)

	if bit != 255 {
		leaves, err := lfjV2ReadTreeLevelU256(read, poolAddress, layout.treeLevel1Slot, key1)
		if err != nil {
			return 0, err
		}
		closestBit := closestBitLeftU256(&leaves, bit)
		if closestBit != 256 {
			newKey2 := key1<<8 | uint32(closestBit)
			leaves2, err := lfjV2ReadTreeLevelU256(read, poolAddress, layout.treeLevel2Slot, newKey2)
			if err != nil {
				return 0, err
			}
			lsb := leastSignificantBitU256(&leaves2)
			return newKey2<<8 | uint32(lsb), nil
		}
	}

	bit = uint8(key1 & 0xFF)
	if bit != 255 {
		val, err := read(poolAddress, big.NewInt(int64(layout.treeLevel0Slot)))
		if err != nil {
			return 0, err
		}
		var leaves uint256.Int
		leaves.SetBytes32(val[:])
		closestBit := closestBitLeftU256(&leaves, bit)
		if closestBit != 256 {
			newKey1 := uint32(closestBit)
			leaves1, err := lfjV2ReadTreeLevelU256(read, poolAddress, layout.treeLevel1Slot, newKey1)
			if err != nil {
				return 0, err
			}
			newKey2 := newKey1<<8 | uint32(leastSignificantBitU256(&leaves1))
			leaves2, err := lfjV2ReadTreeLevelU256(read, poolAddress, layout.treeLevel2Slot, newKey2)
			if err != nil {
				return 0, err
			}
			return newKey2<<8 | uint32(leastSignificantBitU256(&leaves2)), nil
		}
	}

	return 0, nil
}

// ─── State fetch (fast layout) ───

// FetchLFJV2StateFast reads LFJ V2 state, returning a fast layout for use with QuoteLFJV2Fast.
func FetchLFJV2StateFast(read StateReader, poolAddress string, token0, token1 string) (*LFJV2State, *lfjV2LayoutFast, error) {
	// Reuse existing FetchLFJV2StateStorage for the state, then convert layout
	state, origLayout, err := FetchLFJV2StateStorage(read, poolAddress, token0, token1)
	if err != nil {
		return nil, nil, err
	}

	var fastLayout *lfjV2LayoutFast
	if origLayout.parametersSlot.Int64() == 3 {
		fastLayout = &lfjV2LayoutFastA
	} else {
		fastLayout = &lfjV2LayoutFastB
	}

	return state, fastLayout, nil
}

// ─── Main quote function ───

// QuoteLFJV2Fast computes the swap output using uint256 arithmetic.
// Drop-in replacement for QuoteLFJV2Storage.
func QuoteLFJV2Fast(read StateReader, state *LFJV2State, layout *lfjV2LayoutFast, amountIn *big.Int, swapForY bool, blockTimestamp uint64) *big.Int {
	if amountIn.Sign() == 0 {
		return big.NewInt(0)
	}

	var amountInLeft, amountOut uint256.Int
	amountInLeft.SetFromBig(amountIn)

	// Update references based on time
	_, volRef, idRef := updateReferences(
		state.VariableFeeParams.VolatilityAccumulator,
		state.VariableFeeParams.VolatilityReference,
		state.VariableFeeParams.IdReference,
		state.ActiveID,
		state.StaticFeeParams.FilterPeriod,
		state.StaticFeeParams.DecayPeriod,
		state.StaticFeeParams.ReductionFactor,
		state.VariableFeeParams.TimeOfLastUpdate,
		blockTimestamp,
	)

	activeId := state.ActiveID
	binStep := state.BinStep

	for {
		// Read bin reserves via storage
		var reserveX, reserveY uint256.Int
		if cached, ok := state.Bins[activeId]; ok {
			reserveX.SetFromBig(cached.ReserveX)
			reserveY.SetFromBig(cached.ReserveY)
		} else {
			var err error
			reserveX, reserveY, err = lfjV2ReadBinU256(read, state.PoolAddress, layout.binsSlot, activeId)
			if err != nil {
				break
			}
			// Cache in original format for compatibility
			state.Bins[activeId] = LFJV2BinReserves{
				ReserveX: reserveX.ToBig(),
				ReserveY: reserveY.ToBig(),
			}
		}

		var binReserveOut *uint256.Int
		if swapForY {
			binReserveOut = &reserveY
		} else {
			binReserveOut = &reserveX
		}

		if !binReserveOut.IsZero() {
			volAcc := updateVolatilityAccumulator(volRef, idRef, activeId, state.StaticFeeParams.MaxVolatilityAccumulator)
			totalFee := getTotalFeeU256(state.StaticFeeParams.BaseFactor, binStep, volAcc, state.StaticFeeParams.VariableFeeControl)
			price := getPriceFromIdU256(activeId, binStep)

			var maxAmountInNoFee uint256.Int
			if swapForY {
				maxAmountInNoFee = shiftDivU256(binReserveOut, 128, &price, true)
			} else {
				maxAmountInNoFee = mulShiftU256(binReserveOut, &price, 128, true)
			}

			maxFee := getFeeAmountU256(&maxAmountInNoFee, &totalFee)
			var maxAmountIn uint256.Int
			maxAmountIn.Add(&maxAmountInNoFee, &maxFee)

			if !amountInLeft.Lt(&maxAmountIn) {
				// Consume entire bin: amountInLeft >= maxAmountIn
				amountInLeft.Sub(&amountInLeft, &maxAmountIn)
				amountOut.Add(&amountOut, binReserveOut)
			} else {
				// Partial fill
				fee := getFeeAmountFromU256(&amountInLeft, &totalFee)
				var amountAfterFee uint256.Int
				amountAfterFee.Sub(&amountInLeft, &fee)

				var out uint256.Int
				if swapForY {
					out = mulShiftU256(&amountAfterFee, &price, 128, false)
				} else {
					out = shiftDivU256(&amountAfterFee, 128, &price, false)
				}

				if out.Gt(binReserveOut) {
					out.Set(binReserveOut)
				}

				amountOut.Add(&amountOut, &out)
				amountInLeft.Clear()
			}
		}

		if amountInLeft.IsZero() {
			break
		}

		// Move to next bin via tree traversal
		// Check cache first
		var nextId uint32
		var found bool
		if swapForY {
			if next, ok := state.NextBinsDown[activeId]; ok {
				nextId = next
				found = true
			}
		} else {
			if next, ok := state.NextBinsUp[activeId]; ok {
				nextId = next
				found = true
			}
		}

		if !found {
			var err error
			if swapForY {
				nextId, err = lfjV2FindFirstRightU256(read, state.PoolAddress, layout, activeId)
			} else {
				nextId, err = lfjV2FindFirstLeftU256(read, state.PoolAddress, layout, activeId)
			}
			if err != nil {
				// EVM reverts on tree-read error; match that behavior
				return big.NewInt(0)
			}
			if nextId == 0 || nextId == 0xFFFFFF {
				// Out of liquidity — EVM reverts with LBPair__OutOfLiquidity()
				return big.NewInt(0)
			}
			if swapForY {
				state.NextBinsDown[activeId] = nextId
			} else {
				state.NextBinsUp[activeId] = nextId
			}
		}

		activeId = nextId
	}

	return amountOut.ToBig()
}
