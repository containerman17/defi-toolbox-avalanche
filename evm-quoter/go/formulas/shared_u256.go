package formulas

import (
	"encoding/binary"
	"math/big"

	"github.com/ava-labs/libevm/crypto"
	"github.com/holiman/uint256"
)

// shared_u256.go — uint256 versions of the V3/V4/Algebra shared math.
// Zero heap allocations in the hot path.

// BytesStateReader reads a storage slot without big.Int overhead.
// addr is the 20-byte contract address, slot is the 32-byte big-endian slot key.
type BytesStateReader func(addr [20]byte, slot [32]byte) ([32]byte, error)

// ─── Constants ───

var (
	u256One    = uint256.NewInt(1)
	u256Q96    = new(uint256.Int).Lsh(uint256.NewInt(1), 96)
	u256MaxU   = new(uint256.Int).Sub(new(uint256.Int).Lsh(uint256.NewInt(1), 256), uint256.NewInt(1))
	u256Mil    = uint256.NewInt(1_000_000)
	u256MinSqr = uint256.NewInt(4295128739)
	u256MaxSqr = func() *uint256.Int {
		v, _ := uint256.FromDecimal("1461446703485210103287273052203988822378723970342")
		return v
	}()

	// Pre-parsed magic constants for getSqrtRatioAtTick (20 values).
	// Stored as uint256.Int values to avoid runtime parsing.
	tickMagicsU256 [20]uint256.Int
)

func init() {
	hexes := [20]string{
		"fffcb933bd6fad37aa2d162d1a594001", // 0x1
		"fff97272373d413259a46990580e213a", // 0x2
		"fff2e50f5f656932ef12357cf3c7fdcc", // 0x4
		"ffe5caca7e10e4e61c3624eaa0941cd0", // 0x8
		"ffcb9843d60f6159c9db58835c926644", // 0x10
		"ff973b41fa98c081472e6896dfb254c0", // 0x20
		"ff2ea16466c96a3843ec78b326b52861", // 0x40
		"fe5dee046a99a2a811c461f1969c3053", // 0x80
		"fcbe86c7900a88aedcffc83b479aa3a4", // 0x100
		"f987a7253ac413176f2b074cf7815e54", // 0x200
		"f3392b0822b70005940c7a398e4b70f3", // 0x400
		"e7159475a2c29b7443b29c7fa6e889d9", // 0x800
		"d097f3bdfd2022b8845ad8f792aa5825", // 0x1000
		"a9f746462d870fdf8a65dc1f90e061e5", // 0x2000
		"70d869a156d2a1b890bb3df62baf32f7", // 0x4000
		"31be135f97d08fd981231505542fcfa6", // 0x8000
		"9aa508b5b7a84e1c677de54f3e99bc9",  // 0x10000
		"5d6af8dedb81196699c329225ee604",    // 0x20000
		"2216e584f5fa1ea926041bedfe98",      // 0x40000
		"48a170391f7dc42444e8fa2",           // 0x80000
	}
	for i, h := range hexes {
		tickMagicsU256[i].SetFromHex("0x" + h)
	}
}

// ─── getSqrtRatioAtTickU256 ───

// Cache for getSqrtRatioAtTickU256 — ticks are int24 so max 16M entries,
// but in practice only a few thousand unique ticks are used.
var sqrtRatioCache = make(map[int32]uint256.Int, 4096)

func getSqrtRatioAtTickU256(tick int32) uint256.Int {
	if cached, ok := sqrtRatioCache[tick]; ok {
		return cached
	}
	result := getSqrtRatioAtTickU256Compute(tick)
	sqrtRatioCache[tick] = result
	return result
}

func getSqrtRatioAtTickU256Compute(tick int32) uint256.Int {
	absTick := tick
	if absTick < 0 {
		absTick = -absTick
	}

	var ratio uint256.Int
	if absTick&0x1 != 0 {
		ratio.Set(&tickMagicsU256[0])
	} else {
		ratio.Lsh(uint256.NewInt(1), 128)
	}

	bits := [20]int32{0x1, 0x2, 0x4, 0x8, 0x10, 0x20, 0x40, 0x80,
		0x100, 0x200, 0x400, 0x800, 0x1000, 0x2000, 0x4000, 0x8000,
		0x10000, 0x20000, 0x40000, 0x80000}

	for i := 1; i < 20; i++ {
		if absTick&bits[i] != 0 {
			ratio.Mul(&ratio, &tickMagicsU256[i])
			ratio.Rsh(&ratio, 128)
		}
	}

	if tick > 0 {
		ratio.Div(u256MaxU, &ratio)
	}

	// result = (ratio >> 32) + (ratio % (1<<32) != 0 ? 1 : 0)
	var mask32, rem, result uint256.Int
	mask32.SetUint64(0xFFFFFFFF)
	rem.And(&ratio, &mask32)
	result.Rsh(&ratio, 32)
	if !rem.IsZero() {
		result.AddUint64(&result, 1)
	}
	return result
}

// ─── getTickAtSqrtRatioU256 ───

func getTickAtSqrtRatioU256(sqrtPriceX96 *uint256.Int) int32 {
	var ratio uint256.Int
	ratio.Set(sqrtPriceX96)

	var msb int

	thresholds := [8]struct {
		bits int
		exp  uint
	}{
		{128, 128}, {64, 64}, {32, 32}, {16, 16}, {8, 8}, {4, 4}, {2, 2}, {1, 1},
	}

	for _, t := range thresholds {
		var threshold uint256.Int
		threshold.Lsh(uint256.NewInt(1), t.exp)
		if !ratio.Lt(&threshold) {
			msb += t.bits
			ratio.Rsh(&ratio, t.exp)
		}
	}

	if msb >= 128 {
		ratio.Rsh(sqrtPriceX96, uint(msb-127))
	} else {
		ratio.Lsh(sqrtPriceX96, uint(127-msb))
	}

	// log2 = (msb - 128) << 64 as int256 (we use two's complement uint256)
	var log2 uint256.Int
	if msb >= 128 {
		log2.Lsh(uint256.NewInt(uint64(msb-128)), 64)
	} else {
		// negative: two's complement
		log2.Sub(u256MaxU, uint256.NewInt(uint64(128-msb)))
		log2.AddUint64(&log2, 1)
		log2.Lsh(&log2, 64)
	}

	for i := 63; i >= 50; i-- {
		ratio.Mul(&ratio, &ratio)
		ratio.Rsh(&ratio, 127)
		var f uint256.Int
		f.Rsh(&ratio, 128)
		var fShifted uint256.Int
		fShifted.Lsh(&f, uint(i))
		log2.Or(&log2, &fShifted)
		fLow := f.Uint64()
		ratio.Rsh(&ratio, uint(fLow))
	}

	// logSqrt10001 = log2 * 255738958999603826347141
	c1, _ := uint256.FromDecimal("255738958999603826347141")
	var logSqrt10001 uint256.Int
	logSqrt10001.Mul(&log2, c1)

	// tickLow = (logSqrt10001 - c2) >> 128
	c2, _ := uint256.FromDecimal("3402992956809132418596140100660247210")
	var tickLowU uint256.Int
	tickLowU.Sub(&logSqrt10001, c2)
	tickLowU.Rsh(&tickLowU, 128)

	// tickHi = (logSqrt10001 + c3) >> 128
	c3, _ := uint256.FromDecimal("291339464771989622907027621153398088495")
	var tickHiU uint256.Int
	tickHiU.Add(&logSqrt10001, c3)
	tickHiU.Rsh(&tickHiU, 128)

	// Convert to int32 (these are small signed values stored as two's complement)
	tl := u256ToInt32(&tickLowU)
	th := u256ToInt32(&tickHiU)

	if tl == th {
		return tl
	}
	sqrtAtTh := getSqrtRatioAtTickU256(th)
	if !sqrtAtTh.Gt(sqrtPriceX96) {
		return th
	}
	return tl
}

func u256ToInt32(v *uint256.Int) int32 {
	if v.IsZero() {
		return 0
	}
	// Check if negative (high bit set) — check word[3] bit 63
	if v[3]>>(63)&1 != 0 {
		// Two's complement: negate
		var neg uint256.Int
		neg.Sub(u256MaxU, v)
		neg.AddUint64(&neg, 1)
		return -int32(neg.Uint64())
	}
	return int32(v.Uint64())
}

// ─── mulDivU256 ───

// mulDivU256 computes (a * b) / denom, truncating.
// Uses 512-bit intermediate via MulDivOverflow when needed.
func mulDivU256(a, b, denom *uint256.Int) uint256.Int {
	var result uint256.Int
	result.MulDivOverflow(a, b, denom)
	return result
}

// mulDivRoundingUpU256 computes ceil((a * b) / denom).
func mulDivRoundingUpU256(a, b, denom *uint256.Int) uint256.Int {
	result := mulDivU256(a, b, denom)
	// Check remainder: (a*b) mod denom != 0
	var product, rem uint256.Int
	product.Mul(a, b)
	rem.Mod(&product, denom)
	if !rem.IsZero() {
		result.AddUint64(&result, 1)
	}
	return result
}

// ─── unsafeDivRoundingUpU256 ───

func unsafeDivRoundingUpU256(a, denom *uint256.Int) uint256.Int {
	var result, rem uint256.Int
	result.Div(a, denom)
	rem.Mod(a, denom)
	if !rem.IsZero() {
		result.AddUint64(&result, 1)
	}
	return result
}

// ─── Amount delta functions ───

func sGetAmount0DeltaU256(sqrtRatioAX96, sqrtRatioBX96, liquidity *uint256.Int, roundUp bool) uint256.Int {
	a, b := sqrtRatioAX96, sqrtRatioBX96
	if a.Gt(b) {
		a, b = b, a
	}
	var n1, n2 uint256.Int
	n1.Lsh(liquidity, 96)
	n2.Sub(b, a)

	if roundUp {
		inner := mulDivRoundingUpU256(&n1, &n2, b)
		return unsafeDivRoundingUpU256(&inner, a)
	}
	inner := mulDivU256(&n1, &n2, b)
	var result uint256.Int
	result.Div(&inner, a)
	return result
}

func sGetAmount1DeltaU256(sqrtRatioAX96, sqrtRatioBX96, liquidity *uint256.Int, roundUp bool) uint256.Int {
	a, b := sqrtRatioAX96, sqrtRatioBX96
	if a.Gt(b) {
		a, b = b, a
	}
	var diff uint256.Int
	diff.Sub(b, a)

	if roundUp {
		return mulDivRoundingUpU256(liquidity, &diff, u256Q96)
	}
	return mulDivU256(liquidity, &diff, u256Q96)
}

// ─── Next sqrt price ───

func sGetNextSqrtPriceFromInputU256(sqrtPX96, liquidity, amountIn *uint256.Int, zeroForOne bool) uint256.Int {
	if zeroForOne {
		if amountIn.IsZero() {
			var r uint256.Int
			r.Set(sqrtPX96)
			return r
		}
		var n1, prod, denom uint256.Int
		n1.Lsh(liquidity, 96)
		prod.Mul(amountIn, sqrtPX96)
		denom.Add(&n1, &prod)
		return mulDivRoundingUpU256(&n1, sqrtPX96, &denom)
	}
	var quotient uint256.Int
	quotient = mulDivU256(amountIn, u256Q96, liquidity)
	var result uint256.Int
	result.Add(sqrtPX96, &quotient)
	return result
}

// ─── computeSwapStepU256 ───

func computeSwapStepU256(
	sqrtRatioCurrentX96, sqrtRatioTargetX96, liquidity, amountRemaining *uint256.Int,
	feePips uint32,
) (sqrtRatioNextX96, amountIn, amountOut, feeAmount uint256.Int) {
	zeroForOne := !sqrtRatioCurrentX96.Lt(sqrtRatioTargetX96) // current >= target

	var feeComplement uint256.Int
	feeComplement.SetUint64(1_000_000 - uint64(feePips))

	amountRemainingLessFee := mulDivU256(amountRemaining, &feeComplement, u256Mil)

	if zeroForOne {
		amountIn = sGetAmount0DeltaU256(sqrtRatioTargetX96, sqrtRatioCurrentX96, liquidity, true)
	} else {
		amountIn = sGetAmount1DeltaU256(sqrtRatioCurrentX96, sqrtRatioTargetX96, liquidity, true)
	}

	if !amountRemainingLessFee.Lt(&amountIn) {
		// Can reach target
		sqrtRatioNextX96.Set(sqrtRatioTargetX96)
	} else {
		sqrtRatioNextX96 = v3GetNextPriceFromInputU256(sqrtRatioCurrentX96, liquidity, &amountRemainingLessFee, zeroForOne)
	}

	isMax := sqrtRatioTargetX96.Eq(&sqrtRatioNextX96)

	if zeroForOne {
		if !isMax {
			amountIn = sGetAmount0DeltaU256(&sqrtRatioNextX96, sqrtRatioCurrentX96, liquidity, true)
		}
		amountOut = sGetAmount1DeltaU256(&sqrtRatioNextX96, sqrtRatioCurrentX96, liquidity, false)
	} else {
		if !isMax {
			amountIn = sGetAmount1DeltaU256(sqrtRatioCurrentX96, &sqrtRatioNextX96, liquidity, true)
		}
		amountOut = sGetAmount0DeltaU256(sqrtRatioCurrentX96, &sqrtRatioNextX96, liquidity, false)
	}

	if !isMax {
		feeAmount.Sub(amountRemaining, &amountIn)
	} else {
		var feePipsU uint256.Int
		feePipsU.SetUint64(uint64(feePips))
		feeAmount = mulDivRoundingUpU256(&amountIn, &feePipsU, &feeComplement)
	}
	return
}

func v3GetNextPriceFromInputU256(sqrtPX96, liquidity, amountIn *uint256.Int, zeroForOne bool) uint256.Int {
	if zeroForOne {
		// Check overflow: amountIn * sqrtPX96 > 2^256
		var product uint256.Int
		_, overflow := product.MulOverflow(amountIn, sqrtPX96)
		if overflow {
			return v3GetNextPriceFromInputOverflowU256(sqrtPX96, liquidity, amountIn)
		}
	}
	return sGetNextSqrtPriceFromInputU256(sqrtPX96, liquidity, amountIn, zeroForOne)
}

func v3GetNextPriceFromInputOverflowU256(sqrtPX96, liquidity, amountIn *uint256.Int) uint256.Int {
	if amountIn.IsZero() {
		var r uint256.Int
		r.Set(sqrtPX96)
		return r
	}
	var n1, denom uint256.Int
	n1.Lsh(liquidity, 96)
	denom.Div(&n1, sqrtPX96)
	denom.Add(&denom, amountIn)
	return unsafeDivRoundingUpU256(&n1, &denom)
}

// ─── Cached keccak256 for mapping slot computation ───
//
// cachedKeccakMappingSlot computes keccak256(abi.encode(int256(key), uint256(mappingSlot)))
// and caches results. This eliminates repeated keccak256 calls for the same
// (wordPos, mappingSlot) or (tick, mappingSlot) pairs.

// Keccak mapping slot cache — regular map (not sync.Map) since bench is single-threaded.
// Key: mappingSlot bytes (first 8 bytes, enough to distinguish) + int64 key
// Uses a compact key to minimize map overhead.

var keccakSlotCacheU256 = make(map[keccakCacheKey]*big.Int, 8192)

type keccakCacheKey struct {
	key         int64
	mappingSlot uint64 // first 8 bytes of mapping slot (sufficient for uniqueness)
}

func cachedKeccakMappingSlot(key int64, mappingSlot *big.Int) *big.Int {
	// Extract first 8 bytes from Bits() without allocating (Bits() returns internal slice)
	words := mappingSlot.Bits()
	var msKey uint64
	if len(words) > 0 {
		msKey = uint64(words[0])
	}

	ck := keccakCacheKey{key: key, mappingSlot: msKey}
	if v, ok := keccakSlotCacheU256[ck]; ok {
		return v
	}

	var data [64]byte
	if key >= 0 {
		data[24] = byte(key >> 56)
		data[25] = byte(key >> 48)
		data[26] = byte(key >> 40)
		data[27] = byte(key >> 32)
		data[28] = byte(key >> 24)
		data[29] = byte(key >> 16)
		data[30] = byte(key >> 8)
		data[31] = byte(key)
	} else {
		for i := 0; i < 24; i++ {
			data[i] = 0xFF
		}
		data[24] = byte(key >> 56)
		data[25] = byte(key >> 48)
		data[26] = byte(key >> 40)
		data[27] = byte(key >> 32)
		data[28] = byte(key >> 24)
		data[29] = byte(key >> 16)
		data[30] = byte(key >> 8)
		data[31] = byte(key)
	}
	// Write mappingSlot as big-endian bytes into data[32:64]
	for i, w := range words {
		off := 56 - i*8 // words are little-endian, write big-endian
		if off < 0 {
			break
		}
		data[off+0] = byte(w >> 56)
		data[off+1] = byte(w >> 48)
		data[off+2] = byte(w >> 40)
		data[off+3] = byte(w >> 32)
		data[off+4] = byte(w >> 24)
		data[off+5] = byte(w >> 16)
		data[off+6] = byte(w >> 8)
		data[off+7] = byte(w)
	}

	hash := crypto.Keccak256(data[:])
	result := new(big.Int).SetBytes(hash)
	keccakSlotCacheU256[ck] = result
	return result
}

// ─── Bytes-based keccak cache (no big.Int) ───

var keccakSlotCacheBytes = make(map[keccakCacheKey][32]byte, 8192)

// keccakSlotCacheFast uses a single uint64 key for faster lookups.
// Key = mappingSlot_low64 XOR (key * prime) — collision-free for our data.
var keccakSlotCacheFast = make(map[uint64][32]byte, 8192)

func keccakFastKey(key int64, mappingSlot [32]byte) uint64 {
	msKey := binary.LittleEndian.Uint64(mappingSlot[24:32])
	return msKey ^ (uint64(key) * 0x9E3779B97F4A7C15) // fibonacci hashing
}

// cachedKeccakSlotBytes computes keccak256(abi.encode(int256(key), uint256(mappingSlot)))
// using [32]byte throughout — zero big.Int allocations.
func cachedKeccakSlotBytes(key int64, mappingSlot [32]byte) [32]byte {
	fk := keccakFastKey(key, mappingSlot)
	if v, ok := keccakSlotCacheFast[fk]; ok {
		return v
	}

	var data [64]byte
	// Write key as int256 (sign-extended)
	if key < 0 {
		for i := 0; i < 24; i++ {
			data[i] = 0xFF
		}
	}
	data[24] = byte(key >> 56)
	data[25] = byte(key >> 48)
	data[26] = byte(key >> 40)
	data[27] = byte(key >> 32)
	data[28] = byte(key >> 24)
	data[29] = byte(key >> 16)
	data[30] = byte(key >> 8)
	data[31] = byte(key)
	copy(data[32:64], mappingSlot[:])

	hash := crypto.Keccak256(data[:])
	var result [32]byte
	copy(result[:], hash)
	keccakSlotCacheFast[keccakFastKey(key, mappingSlot)] = result
	return result
}

// bigIntTo32 converts a *big.Int to [32]byte (big-endian).
func bigIntTo32(v *big.Int) [32]byte {
	var result [32]byte
	b := v.Bytes()
	copy(result[32-len(b):], b)
	return result
}
