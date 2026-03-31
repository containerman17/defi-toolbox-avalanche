package formulas

import (
	"fmt"
	"math/big"

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

// v4GetPoolStateSlot computes keccak256(abi.encodePacked(poolId, POOLS_SLOT))
func v4GetPoolStateSlot(poolId [32]byte) common.Hash {
	slotPadded := common.LeftPadBytes(big.NewInt(int64(v4PoolsSlot)).Bytes(), 32)
	data := make([]byte, 64)
	copy(data[0:32], poolId[:])
	copy(data[32:64], slotPadded)
	return crypto.Keccak256Hash(data)
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

