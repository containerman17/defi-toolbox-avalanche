package formulas

import (
	"fmt"
	"sync"

	"github.com/holiman/uint256"
)

// v3_u256.go — uint256 version of QuoteV3.
// Uses BytesStateReader for zero big.Int overhead in the hot path.

// v3LayoutBytes holds storage slot offsets as [32]byte for BytesStateReader.
type v3LayoutBytes struct {
	slot0      [32]byte
	liquidity  [32]byte
	ticks      [32]byte
	bitmap     [32]byte
	feeSlot    [32]byte // zero if pool has immutable fees
	hasFeeSlot bool     // true when fee should be read from storage
	heavyGas   bool     // true for PangolinV3/PharaohV3 with extra per-tick overhead
}

var v3LayoutBytesCache sync.Map // map[string]*v3LayoutBytes

func v3GetLayoutBytes(read StateReader, poolAddress string) (*v3LayoutBytes, error) {
	if v, ok := v3LayoutBytesCache.Load(poolAddress); ok {
		return v.(*v3LayoutBytes), nil
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
	if layout.feeSlot != nil {
		lb.feeSlot = bigIntTo32(layout.feeSlot)
		lb.hasFeeSlot = true
	}
	lb.heavyGas = layout.heavyGas
	v3LayoutBytesCache.Store(poolAddress, lb)
	return lb, nil
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

