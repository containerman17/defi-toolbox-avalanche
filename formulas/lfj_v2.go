package formulas

import (
	"fmt"
	"math/big"
	"strings"
	"sync"

	"github.com/ava-labs/libevm/crypto"

)

// LFJ V2 (Trader Joe Liquidity Book) formula.
//
// Uses discrete price bins. Each bin has a fixed price:
//   price = (1 + binStep/10000) ^ (id - 8388608)
// in 128.128 fixed-point format.
//
// Swapping traverses bins, consuming reserves at each bin's price.
// Fee = baseFee + variableFee (from volatility accumulator).

// ─── Constants (matching Solidity Constants.sol) ───

var (
	scaleOffset    = uint(128)
	scale          = new(big.Int).Lsh(big.NewInt(1), scaleOffset)          // 2^128
	precision      = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil) // 1e18
	basisPointMax  = big.NewInt(10000)
	realIDShift    = uint32(1 << 23) // 8388608
	mask128        = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	mask20         = uint32((1 << 20) - 1)
)

// ─── State types ───

type LFJV2BinReserves struct {
	ReserveX *big.Int // uint128
	ReserveY *big.Int // uint128
}

type LFJV2StaticFeeParams struct {
	BaseFactor              uint16
	FilterPeriod            uint16
	DecayPeriod             uint16
	ReductionFactor         uint16
	VariableFeeControl      uint32 // uint24 in Solidity
	ProtocolShare           uint16
	MaxVolatilityAccumulator uint32 // uint24 in Solidity, but actually uint20
}

type LFJV2VariableFeeParams struct {
	VolatilityAccumulator uint32 // uint24 (actually uint20)
	VolatilityReference   uint32 // uint24 (actually uint20)
	IdReference           uint32 // uint24
	TimeOfLastUpdate      uint64 // uint40
}

type LFJV2State struct {
	ActiveID         uint32 // uint24
	BinStep          uint16
	TokenX           string // address of tokenX (lowercase hex)
	StaticFeeParams  LFJV2StaticFeeParams
	VariableFeeParams LFJV2VariableFeeParams
	IsV20            bool   // true if this is a V2.0 (old interface) pool
	PoolAddress      string // pool address for on-demand bin fetching
	// Bins: map from bin ID to reserves (cached, fetched on demand)
	Bins map[uint32]LFJV2BinReserves
	// NextBins: cached next non-empty bin for traversal (fetched on demand)
	NextBinsDown map[uint32]uint32 // binId → next lower non-empty bin
	NextBinsUp   map[uint32]uint32 // binId → next higher non-empty bin
}

// ─── Selectors ───

var (
	getActiveIdSelector           = crypto.Keccak256([]byte("getActiveId()"))[:4]
	getBinStepSelector            = crypto.Keccak256([]byte("getBinStep()"))[:4]
	getStaticFeeParametersSelector = crypto.Keccak256([]byte("getStaticFeeParameters()"))[:4]
	getVariableFeeParametersSelector = crypto.Keccak256([]byte("getVariableFeeParameters()"))[:4]
	getBinSelector                = crypto.Keccak256([]byte("getBin(uint24)"))[:4]
	getNextNonEmptyBinSelector    = crypto.Keccak256([]byte("getNextNonEmptyBin(bool,uint24)"))[:4]
	getTokenXSelector             = crypto.Keccak256([]byte("getTokenX()"))[:4]

	// LFJ V2.0 (old interface) selectors
	v20FeeParametersSelector         = crypto.Keccak256([]byte("feeParameters()"))[:4]
	v20GetReservesAndIdSelector      = crypto.Keccak256([]byte("getReservesAndId()"))[:4]
	v20TokenXSelector                = crypto.Keccak256([]byte("tokenX()"))[:4]
	v20FindFirstNonEmptyBinSelector  = crypto.Keccak256([]byte("findFirstNonEmptyBinId(uint24,bool)"))[:4]
)

// ─── RPC helpers ───









// FetchLFJV2State reads the base state needed to quote swaps for an LFJ V2 pool.
// It reads: activeId, binStep, fee parameters, and tokenX. Bin reserves and
// next-bin lookups are fetched on demand during QuoteLFJV2.

// FetchLFJV2StateV20 reads state from LFJ V2.0 (old interface) pools.
// These use feeParameters(), getReservesAndId(), tokenX(), and
// findFirstNonEmptyBinId(uint24,bool) instead of the V2.1 methods.

// ─── On-demand bin fetching ───

// fetchBinCached fetches bin reserves, using the state cache.

// fetchNextBinCached fetches the next non-empty bin in the given direction, using the state cache.
// swapForY=true → search downward; swapForY=false → search upward.

// ─── 128.128 Fixed-Point Math ───

// mulShift computes (x * y) >> offset using full 512-bit intermediate.
// roundUp: if true, rounds up if there's a remainder.
func mulShift(x, y *big.Int, offset uint, roundUp bool) *big.Int {
	product := new(big.Int).Mul(x, y)
	result := new(big.Int).Rsh(product, offset)
	if roundUp {
		// Check if there's a remainder: product & ((1 << offset) - 1) != 0
		mask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), offset), big.NewInt(1))
		if new(big.Int).And(product, mask).Sign() != 0 {
			result.Add(result, big.NewInt(1))
		}
	}
	return result
}

// shiftDiv computes (x << offset) / denominator.
// roundUp: if true, rounds up.
func shiftDiv(x *big.Int, offset uint, denom *big.Int, roundUp bool) *big.Int {
	shifted := new(big.Int).Lsh(x, offset)
	result := new(big.Int).Div(shifted, denom)
	if roundUp {
		rem := new(big.Int).Mod(shifted, denom)
		if rem.Sign() != 0 {
			result.Add(result, big.NewInt(1))
		}
	}
	return result
}

// pow128 computes base^exponent in 128.128 fixed-point, matching the Solidity
// Uint128x128Math.pow implementation exactly.
func pow128(base *big.Int, exponent int32) *big.Int {
	if exponent == 0 {
		return new(big.Int).Set(scale)
	}

	invert := false
	absY := int64(exponent)
	if absY < 0 {
		absY = -absY
		invert = true
	}

	// If base > 2^128 - 1 (i.e., > mask128), invert base and flip invert flag
	// This matches the Solidity: if gt(x, 0xffffffffffffffffffffffffffffffff)
	squared := new(big.Int).Set(base)
	if squared.Cmp(mask128) > 0 {
		// squared = type(uint256).max / squared
		squared = new(big.Int).Div(maxUint256, squared)
		invert = !invert
	}

	result := new(big.Int).Set(scale)

	// Binary exponentiation - process 20 bits (absY < 0x100000)
	for bit := 0; bit < 20; bit++ {
		if absY&(1<<bit) != 0 {
			// result = (result * squared) >> 128
			result.Mul(result, squared)
			result.Rsh(result, 128)
		}
		// squared = (squared * squared) >> 128
		// (do this for all bits except the last to match Solidity which does it after each check)
		if bit < 19 {
			squared.Mul(squared, squared)
			squared.Rsh(squared, 128)
		}
	}

	if result.Sign() == 0 {
		// Would revert in Solidity
		return big.NewInt(0)
	}

	if invert {
		result = new(big.Int).Div(maxUint256, result)
	}

	return result
}

// getPriceFromId returns the price in 128.128 fixed-point for a given bin ID and bin step.
// Global price cache: key = (binStep, binId) → price (128.128 fixed-point).
// Deterministic: price depends only on binStep and id, never changes.
var binPriceCache sync.Map // map[[2]uint32]*big.Int

func getPriceFromId(id uint32, binStep uint16) *big.Int {
	key := [2]uint32{uint32(binStep), id}
	if cached, ok := binPriceCache.Load(key); ok {
		return new(big.Int).Set(cached.(*big.Int))
	}

	// base = SCALE + (uint256(binStep) << SCALE_OFFSET) / BASIS_POINT_MAX
	// = 2^128 + (binStep * 2^128) / 10000
	bsScaled := new(big.Int).Lsh(big.NewInt(int64(binStep)), scaleOffset)
	bsDiv := new(big.Int).Div(bsScaled, basisPointMax)
	base := new(big.Int).Add(new(big.Int).Set(scale), bsDiv)

	// exponent = int256(uint256(id)) - REAL_ID_SHIFT
	exponent := int32(id) - int32(realIDShift)

	result := pow128(base, exponent)
	binPriceCache.Store(key, new(big.Int).Set(result))
	return result
}

// ─── Fee Calculation ───

// getBaseFee computes: baseFactor * binStep * 1e10
func getBaseFee(baseFactor uint16, binStep uint16) *big.Int {
	return new(big.Int).Mul(
		new(big.Int).Mul(big.NewInt(int64(baseFactor)), big.NewInt(int64(binStep))),
		big.NewInt(1e10),
	)
}

// getVariableFee computes the variable fee from volatility accumulator.
// variableFee = (volAcc * binStep)^2 * variableFeeControl / 100 (rounded up via +99)
func getVariableFee(volAcc uint32, binStep uint16, variableFeeControl uint32) *big.Int {
	if variableFeeControl == 0 {
		return big.NewInt(0)
	}
	prod := new(big.Int).Mul(big.NewInt(int64(volAcc)), big.NewInt(int64(binStep)))
	prodSq := new(big.Int).Mul(prod, prod)
	prodSq.Mul(prodSq, big.NewInt(int64(variableFeeControl)))
	prodSq.Add(prodSq, big.NewInt(99))
	prodSq.Div(prodSq, big.NewInt(100))
	return prodSq
}

// getTotalFee returns baseFee + variableFee, capped to uint128.
func getTotalFee(baseFactor uint16, binStep uint16, volAcc uint32, variableFeeControl uint32) *big.Int {
	bf := getBaseFee(baseFactor, binStep)
	vf := getVariableFee(volAcc, binStep, variableFeeControl)
	total := new(big.Int).Add(bf, vf)
	// safe128: cap to max uint128
	if total.Cmp(mask128) > 0 {
		return new(big.Int).Set(mask128)
	}
	return total
}

// getFeeAmount computes: ceil(amount * totalFee / (PRECISION - totalFee))
func getFeeAmount(amount *big.Int, totalFee *big.Int) *big.Int {
	denom := new(big.Int).Sub(precision, totalFee)
	num := new(big.Int).Mul(amount, totalFee)
	num.Add(num, new(big.Int).Sub(denom, big.NewInt(1)))
	return new(big.Int).Div(num, denom)
}

// getFeeAmountFrom computes: ceil(amountWithFees * totalFee / PRECISION)
func getFeeAmountFrom(amountWithFees *big.Int, totalFee *big.Int) *big.Int {
	num := new(big.Int).Mul(amountWithFees, totalFee)
	num.Add(num, new(big.Int).Sub(precision, big.NewInt(1)))
	return new(big.Int).Div(num, precision)
}

// ─── Volatility Update ───

// updateReferences mirrors the Solidity updateReferences function.
// It updates the volatility reference and ID reference based on time elapsed.
func updateReferences(
	volAcc, volRef, idRef uint32,
	activeId uint32,
	filterPeriod, decayPeriod, reductionFactor uint16,
	timeLastUpdate uint64,
	blockTimestamp uint64,
) (newVolAcc, newVolRef, newIdRef uint32) {
	newVolAcc = volAcc
	newVolRef = volRef
	newIdRef = idRef

	dt := blockTimestamp - timeLastUpdate

	if dt >= uint64(filterPeriod) {
		// updateIdReference: idRef = activeId
		newIdRef = activeId

		if dt < uint64(decayPeriod) {
			// updateVolatilityReference: volRef = volAcc * reductionFactor / 10000
			newVolRef = uint32(uint64(volAcc) * uint64(reductionFactor) / 10000)
		} else {
			newVolRef = 0
		}
	}

	return newVolAcc, newVolRef, newIdRef
}

// updateVolatilityAccumulator computes the new volatility accumulator when
// visiting a bin during the swap.
func updateVolatilityAccumulator(
	volRef uint32, idRef uint32, activeId uint32, maxVolAcc uint32,
) uint32 {
	var deltaId uint32
	if activeId > idRef {
		deltaId = activeId - idRef
	} else {
		deltaId = idRef - activeId
	}

	volAcc := uint64(volRef) + uint64(deltaId)*10000 // deltaId * BASIS_POINT_MAX
	if volAcc > uint64(maxVolAcc) {
		volAcc = uint64(maxVolAcc)
	}
	return uint32(volAcc) & mask20
}

// ─── Storage Layout ───
//
// LFJ V2.1 (LBPair) uses a delegatecall proxy pattern. The storage layout has two
// variants depending on the implementation contract:
//
//   Layout A (impl 0x7a5b...): _parameters=slot 3, _bins=slot 6, tree={8,9,10}
//   Layout B (impls 0x3e30/0xee5a): _parameters=slot 4, _bins=slot 7, tree={8,9,10}
//
// The _parameters slot packs all fee parameters + activeId + volatility state in
// a single bytes32 using the PairParameterHelper bit-field layout:
//   [0:16]   baseFactor
//   [16:28]  filterPeriod (12 bits)
//   [28:40]  decayPeriod (12 bits)
//   [40:54]  reductionFactor (14 bits)
//   [54:78]  variableFeeControl (24 bits)
//   [78:92]  protocolShare (14 bits)
//   [92:112] maxVolatilityAccumulator (20 bits)
//   [112:132] volatilityAccumulator (20 bits)
//   [132:152] volatilityReference (20 bits)
//   [152:176] idReference (24 bits)
//   [176:216] timeOfLastUpdate (40 bits)
//   [216:232] oracleId (16 bits)
//   [232:256] activeId (24 bits)
//
// The _bins mapping (keccak256(binId, binsSlot) → bytes32) packs:
//   lower 128 bits = reserveX, upper 128 bits = reserveY
//
// The tree structure for findFirstNonEmptyBin uses 3 levels:
//   level0 = slot 8 (single bytes32 root bitmap)
//   level1 = slot 9 (mapping: keccak256(key, 9) → bytes32)
//   level2 = slot 10 (mapping: keccak256(key, 10) → bytes32)

type lfjV2Layout struct {
	parametersSlot *big.Int
	binsSlot       *big.Int
	treeLevel0Slot *big.Int
	treeLevel1Slot *big.Int
	treeLevel2Slot *big.Int
}

var (
	lfjV2LayoutA = lfjV2Layout{
		parametersSlot: big.NewInt(3),
		binsSlot:       big.NewInt(6),
		treeLevel0Slot: big.NewInt(7),
		treeLevel1Slot: big.NewInt(8),
		treeLevel2Slot: big.NewInt(9),
	}
	lfjV2LayoutB = lfjV2Layout{
		parametersSlot: big.NewInt(4),
		binsSlot:       big.NewInt(7),
		treeLevel0Slot: big.NewInt(8),
		treeLevel1Slot: big.NewInt(9),
		treeLevel2Slot: big.NewInt(10),
	}
)

// lfjV2DetectLayout detects the storage layout by reading _parameters from slot 3 or 4.
// Returns the layout and the decoded parameters value.
func lfjV2DetectLayout(read StateReader, poolAddress string) (*lfjV2Layout, *big.Int, error) {
	// Try Layout A first (slot 3) - most common (~74%)
	val, err := read(poolAddress, lfjV2LayoutA.parametersSlot)
	if err != nil {
		return nil, nil, err
	}
	v := new(big.Int).SetBytes(val[:])
	activeId := uint32((v.Uint64() >> 40) & 0xFFFFFF) // rough check: activeId at bits 232
	// Need to check higher bits since activeId is at bits 232-255
	vBig := new(big.Int).SetBytes(val[:])
	activeIdBig := new(big.Int).Rsh(vBig, 232)
	activeIdBig.And(activeIdBig, big.NewInt(0xFFFFFF))
	activeId = uint32(activeIdBig.Uint64())

	if activeId > 0 && activeId < 0xFFFFFF {
		return &lfjV2LayoutA, vBig, nil
	}

	// Try Layout B (slot 4)
	val, err = read(poolAddress, lfjV2LayoutB.parametersSlot)
	if err != nil {
		return nil, nil, err
	}
	vBig = new(big.Int).SetBytes(val[:])
	activeIdBig = new(big.Int).Rsh(new(big.Int).SetBytes(val[:]), 232)
	activeIdBig.And(activeIdBig, big.NewInt(0xFFFFFF))
	activeId = uint32(activeIdBig.Uint64())

	if activeId > 0 && activeId < 0xFFFFFF {
		return &lfjV2LayoutB, vBig, nil
	}

	return nil, nil, fmt.Errorf("could not detect LFJ V2 layout for %s (activeId=%d)", poolAddress, activeId)
}

// lfjV2DecodeParameters extracts all parameters from the packed _parameters bytes32.
func lfjV2DecodeParameters(v *big.Int) (activeId uint32, staticParams LFJV2StaticFeeParams, varParams LFJV2VariableFeeParams) {
	// Extract fields from the packed value using bit shifts and masks
	baseFactor := uint16(new(big.Int).And(v, big.NewInt(0xFFFF)).Uint64())
	filterPeriod := uint16(new(big.Int).And(new(big.Int).Rsh(v, 16), big.NewInt(0xFFF)).Uint64())
	decayPeriod := uint16(new(big.Int).And(new(big.Int).Rsh(v, 28), big.NewInt(0xFFF)).Uint64())
	reductionFactor := uint16(new(big.Int).And(new(big.Int).Rsh(v, 40), big.NewInt(0x3FFF)).Uint64())
	varFeeControl := uint32(new(big.Int).And(new(big.Int).Rsh(v, 54), big.NewInt(0xFFFFFF)).Uint64())
	protocolShare := uint16(new(big.Int).And(new(big.Int).Rsh(v, 78), big.NewInt(0x3FFF)).Uint64())
	maxVolAcc := uint32(new(big.Int).And(new(big.Int).Rsh(v, 92), big.NewInt(0xFFFFF)).Uint64())
	volAcc := uint32(new(big.Int).And(new(big.Int).Rsh(v, 112), big.NewInt(0xFFFFF)).Uint64())
	volRef := uint32(new(big.Int).And(new(big.Int).Rsh(v, 132), big.NewInt(0xFFFFF)).Uint64())
	idRef := uint32(new(big.Int).And(new(big.Int).Rsh(v, 152), big.NewInt(0xFFFFFF)).Uint64())
	timeLastUpdate := new(big.Int).And(new(big.Int).Rsh(v, 176), big.NewInt(0xFFFFFFFFFF)).Uint64()
	activeId = uint32(new(big.Int).And(new(big.Int).Rsh(v, 232), big.NewInt(0xFFFFFF)).Uint64())

	staticParams = LFJV2StaticFeeParams{
		BaseFactor:               baseFactor,
		FilterPeriod:             filterPeriod,
		DecayPeriod:              decayPeriod,
		ReductionFactor:          reductionFactor,
		VariableFeeControl:       varFeeControl,
		ProtocolShare:            protocolShare,
		MaxVolatilityAccumulator: maxVolAcc,
	}
	varParams = LFJV2VariableFeeParams{
		VolatilityAccumulator: volAcc,
		VolatilityReference:   volRef,
		IdReference:           idRef,
		TimeOfLastUpdate:      timeLastUpdate,
	}
	return
}

// lfjV2ReadBin reads bin reserves from storage using the StateReader.
// The bins mapping: keccak256(abi.encode(binId, binsSlot)) → packed uint128:uint128
func lfjV2ReadBin(read StateReader, poolAddress string, binsSlot *big.Int, binID uint32) (LFJV2BinReserves, error) {
	// Compute mapping slot: keccak256(uint256(binID) || uint256(binsSlot))
	data := make([]byte, 64)
	big.NewInt(int64(binID)).FillBytes(data[0:32])
	binsSlot.FillBytes(data[32:64])
	slot := new(big.Int).SetBytes(crypto.Keccak256(data))

	val, err := read(poolAddress, slot)
	if err != nil {
		return LFJV2BinReserves{}, err
	}

	packed := new(big.Int).SetBytes(val[:])
	reserveX := new(big.Int).And(packed, mask128)
	reserveY := new(big.Int).Rsh(packed, 128)

	return LFJV2BinReserves{
		ReserveX: reserveX,
		ReserveY: reserveY,
	}, nil
}

// ─── Tree Traversal (BitMath) ───

// lfjV2MostSignificantBit returns the position of the highest set bit in x.
func lfjV2MostSignificantBit(x *big.Int) uint8 {
	if x.Sign() == 0 {
		return 0
	}
	return uint8(x.BitLen() - 1)
}

// lfjV2LeastSignificantBit returns the position of the lowest set bit in x.
func lfjV2LeastSignificantBit(x *big.Int) uint8 {
	if x.Sign() == 0 {
		return 255
	}
	// Find lowest set bit: position of the least significant 1
	for i := uint8(0); i < 255; i++ {
		if x.Bit(int(i)) != 0 {
			return i
		}
	}
	return 255
}

// lfjV2ClosestBitRight finds the closest set bit STRICTLY to the right of position 'bit'.
// Matches TreeMath._closestBitRight which calls BitMath.closestBitRight(leaves, bit - 1).
// The (bit-1) adjustment means: shift = 255 - (bit-1) = 256 - bit.
// Returns 256 (sentinel for type(uint256).max) if none found.
func lfjV2ClosestBitRight(x *big.Int, bit uint8) int {
	if bit == 0 {
		return 256 // no bits strictly to the right of position 0
	}
	// BitMath.closestBitRight(x, bit - 1): shift = 255 - (bit - 1) = 256 - bit
	shift := uint(256 - uint(bit))
	shifted := new(big.Int).Lsh(x, shift)
	// Mask to 256 bits (Solidity uint256 overflow semantics)
	shifted.And(shifted, maxUint256)
	if shifted.Sign() == 0 {
		return 256
	}
	msb := lfjV2MostSignificantBit(shifted)
	return int(msb) - int(shift)
}

// lfjV2ClosestBitLeft finds the closest set bit STRICTLY to the left of position 'bit'.
// Matches TreeMath._closestBitLeft which calls BitMath.closestBitLeft(leaves, bit + 1).
// The (bit+1) adjustment is applied to the shift.
// Returns 256 (sentinel for type(uint256).max) if none found.
func lfjV2ClosestBitLeft(x *big.Int, bit uint8) int {
	if bit >= 255 {
		return 256 // no bits strictly to the left of position 255
	}
	// BitMath.closestBitLeft(x, bit + 1): shift right by (bit + 1)
	shift := uint(bit + 1)
	shifted := new(big.Int).Rsh(x, shift)
	if shifted.Sign() == 0 {
		return 256
	}
	lsb := lfjV2LeastSignificantBit(shifted)
	return int(lsb) + int(shift)
}

// lfjV2ReadTreeLevel reads a tree level mapping value.
func lfjV2ReadTreeLevel(read StateReader, poolAddress string, levelSlot *big.Int, key *big.Int) (*big.Int, error) {
	data := make([]byte, 64)
	key.FillBytes(data[0:32])
	levelSlot.FillBytes(data[32:64])
	slot := new(big.Int).SetBytes(crypto.Keccak256(data))
	val, err := read(poolAddress, slot)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(val[:]), nil
}

// lfjV2FindFirstRight finds the next non-empty bin with a LOWER ID than 'id'.
// Replicates TreeMath.findFirstRight exactly.
func lfjV2FindFirstRight(read StateReader, poolAddress string, layout *lfjV2Layout, id uint32) (uint32, error) {
	key2 := uint32(id) >> 8
	bit := uint8(id & 0xFF)

	if bit != 0 {
		leaves, err := lfjV2ReadTreeLevel(read, poolAddress, layout.treeLevel2Slot, big.NewInt(int64(key2)))
		if err != nil {
			return 0, err
		}
		closestBit := lfjV2ClosestBitRight(leaves, bit)
		if closestBit != 256 {
			return uint32(key2)<<8 | uint32(closestBit), nil
		}
	}

	key1 := key2 >> 8
	bit = uint8(key2 & 0xFF)

	if bit != 0 {
		leaves, err := lfjV2ReadTreeLevel(read, poolAddress, layout.treeLevel1Slot, big.NewInt(int64(key1)))
		if err != nil {
			return 0, err
		}
		closestBit := lfjV2ClosestBitRight(leaves, bit)
		if closestBit != 256 {
			newKey2 := uint32(key1)<<8 | uint32(closestBit)
			leaves2, err := lfjV2ReadTreeLevel(read, poolAddress, layout.treeLevel2Slot, big.NewInt(int64(newKey2)))
			if err != nil {
				return 0, err
			}
			msb := lfjV2MostSignificantBit(leaves2)
			return newKey2<<8 | uint32(msb), nil
		}
	}

	bit = uint8(key1 & 0xFF)
	if bit != 0 {
		// Read level0 directly from its slot (not a mapping)
		val, err := read(poolAddress, layout.treeLevel0Slot)
		if err != nil {
			return 0, err
		}
		leaves := new(big.Int).SetBytes(val[:])
		closestBit := lfjV2ClosestBitRight(leaves, bit)
		if closestBit != 256 {
			newKey1 := uint32(closestBit)
			leaves1, err := lfjV2ReadTreeLevel(read, poolAddress, layout.treeLevel1Slot, big.NewInt(int64(newKey1)))
			if err != nil {
				return 0, err
			}
			newKey2 := newKey1<<8 | uint32(lfjV2MostSignificantBit(leaves1))
			leaves2, err := lfjV2ReadTreeLevel(read, poolAddress, layout.treeLevel2Slot, big.NewInt(int64(newKey2)))
			if err != nil {
				return 0, err
			}
			return newKey2<<8 | uint32(lfjV2MostSignificantBit(leaves2)), nil
		}
	}

	return 0xFFFFFF, nil // type(uint24).max - no bin found
}

// lfjV2FindFirstLeft finds the next non-empty bin with a HIGHER ID than 'id'.
// Replicates TreeMath.findFirstLeft exactly.
func lfjV2FindFirstLeft(read StateReader, poolAddress string, layout *lfjV2Layout, id uint32) (uint32, error) {
	key2 := uint32(id) >> 8
	bit := uint8(id & 0xFF)

	if bit != 255 {
		leaves, err := lfjV2ReadTreeLevel(read, poolAddress, layout.treeLevel2Slot, big.NewInt(int64(key2)))
		if err != nil {
			return 0, err
		}
		closestBit := lfjV2ClosestBitLeft(leaves, bit)
		if closestBit != 256 {
			return uint32(key2)<<8 | uint32(closestBit), nil
		}
	}

	key1 := key2 >> 8
	bit = uint8(key2 & 0xFF)

	if bit != 255 {
		leaves, err := lfjV2ReadTreeLevel(read, poolAddress, layout.treeLevel1Slot, big.NewInt(int64(key1)))
		if err != nil {
			return 0, err
		}
		closestBit := lfjV2ClosestBitLeft(leaves, bit)
		if closestBit != 256 {
			newKey2 := uint32(key1)<<8 | uint32(closestBit)
			leaves2, err := lfjV2ReadTreeLevel(read, poolAddress, layout.treeLevel2Slot, big.NewInt(int64(newKey2)))
			if err != nil {
				return 0, err
			}
			lsb := lfjV2LeastSignificantBit(leaves2)
			return newKey2<<8 | uint32(lsb), nil
		}
	}

	bit = uint8(key1 & 0xFF)
	if bit != 255 {
		val, err := read(poolAddress, layout.treeLevel0Slot)
		if err != nil {
			return 0, err
		}
		leaves := new(big.Int).SetBytes(val[:])
		closestBit := lfjV2ClosestBitLeft(leaves, bit)
		if closestBit != 256 {
			newKey1 := uint32(closestBit)
			leaves1, err := lfjV2ReadTreeLevel(read, poolAddress, layout.treeLevel1Slot, big.NewInt(int64(newKey1)))
			if err != nil {
				return 0, err
			}
			newKey2 := newKey1<<8 | uint32(lfjV2LeastSignificantBit(leaves1))
			leaves2, err := lfjV2ReadTreeLevel(read, poolAddress, layout.treeLevel2Slot, big.NewInt(int64(newKey2)))
			if err != nil {
				return 0, err
			}
			return newKey2<<8 | uint32(lfjV2LeastSignificantBit(leaves2)), nil
		}
	}

	return 0, nil // 0 means no bin found (findFirstLeft returns 0 on failure)
}

// FetchLFJV2StateStorage reads LFJ V2 state using direct storage reads.
// binStep and tokenX are looked up from the hardcoded registry (immutables that never change).
// All other state (_parameters, bins, tree) is read from storage.
func FetchLFJV2StateStorage(read StateReader, poolAddress string, token0, token1 string) (*LFJV2State, *lfjV2Layout, error) {
	// Look up immutables from registry
	imm, ok := lfjV2Registry[strings.ToLower(poolAddress)]
	if !ok {
		return nil, nil, fmt.Errorf("pool %s not in LFJ V2 registry", poolAddress)
	}

	// Detect layout and read _parameters in one step
	layout, paramVal, err := lfjV2DetectLayout(read, poolAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("detect layout: %w", err)
	}

	activeId, staticParams, varParams := lfjV2DecodeParameters(paramVal)

	// Derive tokenX from registry
	tokenX := strings.ToLower(token0)
	if !imm.TokenXIsToken0 {
		tokenX = strings.ToLower(token1)
	}

	state := &LFJV2State{
		ActiveID:          activeId,
		BinStep:           imm.BinStep,
		TokenX:            tokenX,
		StaticFeeParams:   staticParams,
		VariableFeeParams: varParams,
		IsV20:             false,
		PoolAddress:       poolAddress,
		Bins:              make(map[uint32]LFJV2BinReserves),
		NextBinsDown:      make(map[uint32]uint32),
		NextBinsUp:        make(map[uint32]uint32),
	}

	return state, layout, nil
}

// QuoteLFJV2Storage computes the swap output using storage reads for bins and tree.
func QuoteLFJV2Storage(read StateReader, state *LFJV2State, layout *lfjV2Layout, amountIn *big.Int, swapForY bool, blockTimestamp uint64) *big.Int {
	if amountIn.Sign() == 0 {
		return big.NewInt(0)
	}

	amountInLeft := new(big.Int).Set(amountIn)
	amountOut := big.NewInt(0)

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
		// Read bin from storage (cached after first read)
		var binRes LFJV2BinReserves
		if cached, ok := state.Bins[activeId]; ok {
			binRes = cached
		} else {
			var err error
			binRes, err = lfjV2ReadBin(read, state.PoolAddress, layout.binsSlot, activeId)
			if err != nil {
				break
			}
			state.Bins[activeId] = binRes
		}

		// Check if bin has reserves on the output side
		var binReserveOut *big.Int
		if swapForY {
			binReserveOut = binRes.ReserveY
		} else {
			binReserveOut = binRes.ReserveX
		}

		if binReserveOut != nil && binReserveOut.Sign() > 0 {
			volAcc := updateVolatilityAccumulator(
				volRef, idRef, activeId,
				state.StaticFeeParams.MaxVolatilityAccumulator,
			)

			totalFee := getTotalFee(
				state.StaticFeeParams.BaseFactor,
				binStep,
				volAcc,
				state.StaticFeeParams.VariableFeeControl,
			)

			price := getPriceFromId(activeId, binStep)

			var maxAmountInNoFee *big.Int
			if swapForY {
				maxAmountInNoFee = shiftDiv(binReserveOut, scaleOffset, price, true)
			} else {
				maxAmountInNoFee = mulShift(binReserveOut, price, scaleOffset, true)
			}

			maxFee := getFeeAmount(maxAmountInNoFee, totalFee)
			maxAmountIn := new(big.Int).Add(maxAmountInNoFee, maxFee)

			var amountIn128, fee128 *big.Int
			var amountOut128 *big.Int

			if amountInLeft.Cmp(maxAmountIn) >= 0 {
				fee128 = new(big.Int).Set(maxFee)
				amountIn128 = new(big.Int).Set(maxAmountIn)
				amountOut128 = new(big.Int).Set(binReserveOut)
			} else {
				amountIn128 = new(big.Int).Set(amountInLeft)
				fee128 = getFeeAmountFrom(amountIn128, totalFee)
				amountAfterFee := new(big.Int).Sub(amountIn128, fee128)

				if swapForY {
					amountOut128 = mulShift(amountAfterFee, price, scaleOffset, false)
				} else {
					amountOut128 = shiftDiv(amountAfterFee, scaleOffset, price, false)
				}

				if amountOut128.Cmp(binReserveOut) > 0 {
					amountOut128 = new(big.Int).Set(binReserveOut)
				}
			}

			if amountIn128.Sign() > 0 {
				amountInLeft.Sub(amountInLeft, amountIn128)
				amountOut.Add(amountOut, amountOut128)
			}

			_ = fee128
		}

		if amountInLeft.Sign() == 0 {
			break
		}

		// Move to next bin using tree traversal via storage reads
		var nextId uint32
		var found bool

		// Check cache first
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
				nextId, err = lfjV2FindFirstRight(read, state.PoolAddress, layout, activeId)
			} else {
				nextId, err = lfjV2FindFirstLeft(read, state.PoolAddress, layout, activeId)
			}
			if err != nil {
				break
			}
			if nextId == 0 || nextId == 0xFFFFFF {
				break
			}
			// Cache
			if swapForY {
				state.NextBinsDown[activeId] = nextId
			} else {
				state.NextBinsUp[activeId] = nextId
			}
		}

		activeId = nextId
	}

	return amountOut
}

// ─── Main Quote Function (legacy eth_call version) ───

// QuoteLFJV2 computes the swap output for an LFJ V2 pool.
// Bins and next-bin lookups are fetched on demand from the RPC pool and cached in state.
// swapForY: true if swapping token X → token Y (zeroForOne).
// blockTimestamp: the block.timestamp used for volatility reference updates.
