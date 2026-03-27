package formulas

import (
	"math/big"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
	"github.com/holiman/uint256"
)

// V4Pool is a pre-loaded Uniswap V4 pool.
// Similar to V3Pool but reads from the PoolManager singleton contract.
type V4Pool struct {
	addr         common.Address // pseudo-address for the pool (used as key)
	poolId       [32]byte
	tickSpacing  int32
	hookFeePpm   uint32
	lpFee        uint32 // from on-chain slot0 (LP fee in ppm, e.g. 500 = 0.05%)
	protocolFee  uint32 // from on-chain slot0 (24-bit packed: lower 12 = 0→1, upper 12 = 1→0)
	sqrtPriceX96 uint256.Int
	tick         int32
	liquidity    uint256.Int

	// Pre-loaded bitmap words: wordPos -> 256-bit bitmap
	bitmapWords map[int16]uint256.Int

	// Pre-loaded tick data: tickIdx -> liquidityNet
	tickLiquidityNet map[int32]uint256.Int
}

func newV4Pool(addr common.Address, reader StorageReader) *V4Pool {
	poolAddress := poolHex(addr)

	info, ok := v4PoolIds[poolAddress]
	if !ok {
		return nil
	}

	bytesReader := func(a [20]byte, slot [32]byte) ([32]byte, error) {
		return reader(common.Address(a), common.Hash(slot)), nil
	}

	// Read ArenaHook fee from storage if hooks == arenaHookAddress
	hookFeePpm := info.hookFeePpm
	if hookFeePpm == 0 && info.hooks == common.HexToAddress(arenaHookAddress) {
		hookFeePpm = readArenaHookFee(reader, info.poolId)
	}

	state, err := FetchV4StateFromBytes(bytesReader, info.poolId, info.tickSpacing, hookFeePpm)
	if err != nil || state.SqrtPriceX96.Sign() == 0 {
		// Pool is uninitialized (zero sqrtPrice) or fetch failed.
		// Return an empty V4Pool whose Quote() returns (nil, false), preventing
		// EVM fallback for pools that are simply empty — EVM would also return zero.
		return &V4Pool{
			addr:             addr,
			poolId:           info.poolId,
			tickSpacing:      info.tickSpacing,
			hookFeePpm:       hookFeePpm,
			bitmapWords:      make(map[int16]uint256.Int),
			tickLiquidityNet: make(map[int32]uint256.Int),
		}
	}

	stateSlot := v4GetPoolStateSlot(info.poolId)
	bitmapSlotBase := v4AddOffset32(stateSlot, v4TickBitmapOffset)
	ticksSlotBase := v4AddOffset32(stateSlot, v4TicksOffset)

	pmAddr := v4PoolManagerAddr

	// Pre-load bitmap words (±200 range)
	bitmapWords := make(map[int16]uint256.Int)
	for wordPos := int16(-200); wordPos <= 200; wordPos++ {
		slotKey := cachedKeccakSlotBytes(int64(wordPos), bitmapSlotBase)
		data, err := bytesReader(pmAddr, slotKey)
		if err != nil {
			continue
		}
		var word uint256.Int
		word.SetBytes32(data[:])
		if !word.IsZero() {
			bitmapWords[wordPos] = word
		}
	}

	// Pre-load liquidityNet for all initialized ticks
	tickLiquidityNet := make(map[int32]uint256.Int)
	for wordPos, word := range bitmapWords {
		for bit := 0; bit < 256; bit++ {
			if word[bit/64]&(1<<uint(bit%64)) != 0 {
				tickIdx := (int32(wordPos)*256 + int32(bit)) * info.tickSpacing
				slotKey := cachedKeccakSlotBytes(int64(tickIdx), ticksSlotBase)
				data, err := bytesReader(pmAddr, slotKey)
				if err != nil {
					continue
				}
				var val uint256.Int
				val.SetBytes32(data[:])
				val.Rsh(&val, 128)
				val.And(&val, mask128U256)
				// Sign-extend from 128 bits
				if val[1]>>(63)&1 != 0 {
					var highMask uint256.Int
					highMask.Lsh(u256MaxU, 128)
					val.Or(&val, &highMask)
				}
				tickLiquidityNet[tickIdx] = val
			}
		}
	}

	pool := &V4Pool{
		addr:             addr,
		poolId:           info.poolId,
		tickSpacing:      info.tickSpacing,
		hookFeePpm:       hookFeePpm,
		lpFee:            state.LpFee,
		protocolFee:      state.ProtocolFee,
		bitmapWords:      bitmapWords,
		tickLiquidityNet: tickLiquidityNet,
	}
	pool.sqrtPriceX96.SetFromBig(state.SqrtPriceX96)
	pool.tick = state.Tick
	pool.liquidity.SetFromBig(state.Liquidity)

	return pool
}

func (p *V4Pool) Address() common.Address {
	return p.addr
}

func (p *V4Pool) Quote(amountIn *uint256.Int, zeroForOne bool) uint256.Int {
	if amountIn.IsZero() || p.sqrtPriceX96.IsZero() {
		return uint256.Int{}
	}

	// Determine swap fee per-direction using stored lpFee and protocolFee.
	var protocolFee uint32
	if zeroForOne {
		protocolFee = p.protocolFee & 0xFFF
	} else {
		protocolFee = (p.protocolFee >> 12) & 0xFFF
	}

	var swapFee uint32
	if protocolFee == 0 {
		swapFee = p.lpFee
	} else {
		swapFee = protocolFee + p.lpFee - (protocolFee*p.lpFee)/uint32(v4MaxSwapFee)
	}

	// Price limit
	var sqrtPriceLimitX96 uint256.Int
	if zeroForOne {
		sqrtPriceLimitX96.Add(u256MinSqr, uint256.NewInt(1))
	} else {
		sqrtPriceLimitX96.Sub(u256MaxSqr, uint256.NewInt(1))
	}

	var sqrtPriceX96, liquidity, absRemaining, amountOut uint256.Int
	sqrtPriceX96.Set(&p.sqrtPriceX96)
	liquidity.Set(&p.liquidity)
	absRemaining.Set(amountIn)
	tick := p.tick

	for !absRemaining.IsZero() && !sqrtPriceX96.Eq(&sqrtPriceLimitX96) {
		nextTick, initialized := p.nextInitializedTick(tick, zeroForOne)

		if nextTick < algebraMinTick {
			nextTick = algebraMinTick
		}
		if nextTick > algebraMaxTick {
			nextTick = algebraMaxTick
		}

		sqrtPriceNextX96 := getSqrtRatioAtTickU256(nextTick)

		var sqrtPriceTargetX96 *uint256.Int
		if zeroForOne {
			if sqrtPriceNextX96.Lt(&sqrtPriceLimitX96) {
				sqrtPriceTargetX96 = &sqrtPriceLimitX96
			} else {
				sqrtPriceTargetX96 = &sqrtPriceNextX96
			}
		} else {
			if sqrtPriceLimitX96.Lt(&sqrtPriceNextX96) {
				sqrtPriceTargetX96 = &sqrtPriceLimitX96
			} else {
				sqrtPriceTargetX96 = &sqrtPriceNextX96
			}
		}

		newSqrtPriceX96, stepAmountIn, stepAmountOut, stepFeeAmount := v4ComputeSwapStepU256(
			&sqrtPriceX96, sqrtPriceTargetX96, &liquidity, &absRemaining, swapFee,
		)
		sqrtPriceX96.Set(&newSqrtPriceX96)

		var consumed uint256.Int
		consumed.Add(&stepAmountIn, &stepFeeAmount)
		absRemaining.Sub(&absRemaining, &consumed)
		amountOut.Add(&amountOut, &stepAmountOut)

		if sqrtPriceX96.Eq(&sqrtPriceNextX96) {
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

	// Apply ArenaHook fee
	if p.hookFeePpm > 0 && !amountOut.IsZero() {
		var feePpm, maxFee uint256.Int
		feePpm.SetUint64(uint64(p.hookFeePpm))
		maxFee.SetUint64(arenaHookMaxFee)
		feeAmt := mulDivU256(&amountOut, &feePpm, &maxFee)
		amountOut.Sub(&amountOut, &feeAmt)
	}

	return amountOut
}

// nextInitializedTick scans one pre-loaded bitmap word (same as V3Pool).
func (p *V4Pool) nextInitializedTick(tick int32, zeroForOne bool) (int32, bool) {
	compressed := v4Compress(tick, p.tickSpacing)

	if zeroForOne {
		wordPos, bitPos := v4Position(compressed)
		word := p.bitmapWords[wordPos]

		var mask, masked uint256.Int
		mask.Lsh(uint256.NewInt(1), uint(bitPos)+1)
		mask.SubUint64(&mask, 1)
		masked.And(&word, &mask)

		if !masked.IsZero() {
			msb := masked.BitLen() - 1
			next := int32((int(compressed) - (int(bitPos) - msb)) * int(p.tickSpacing))
			return next, true
		}
		next := int32((int(compressed) - int(bitPos)) * int(p.tickSpacing))
		return next, false
	}

	compressed++
	wordPos, bitPos := v4Position(compressed)
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
		next := int32((int(compressed) + (lsb - int(bitPos))) * int(p.tickSpacing))
		return next, true
	}
	next := int32((int(compressed) + (255 - int(bitPos))) * int(p.tickSpacing))
	return next, false
}

// v4PoolInfo holds the parameters needed to construct a V4Pool.
type v4PoolInfo struct {
	poolId      [32]byte
	tickSpacing int32
	hookFeePpm  uint32
	lpFee       uint32
	hooks       common.Address
}

// v4PoolIds maps pool pseudo-address (lowercase hex) to pool parameters.
// Populated by the pool registry / discovery process.
var v4PoolIds = map[string]*v4PoolInfo{}

// RegisterV4Pool registers a V4 pool's parameters for construction.
func RegisterV4Pool(poolAddress string, poolId [32]byte, tickSpacing int32, lpFee uint32, hookFeePpm uint32, hooks common.Address) {
	v4PoolIds[poolAddress] = &v4PoolInfo{
		poolId:      poolId,
		tickSpacing: tickSpacing,
		hookFeePpm:  hookFeePpm,
		lpFee:       lpFee,
		hooks:       hooks,
	}
}

// readArenaHookFee reads the ArenaHook's total fee (pool-specific + protocol) from storage.
// Returns the fee in ppm (parts per million), e.g. 2000 = 0.2%.
func readArenaHookFee(reader StorageReader, poolId [32]byte) uint32 {
	feeHelperAddr := common.HexToAddress(arenaFeeHelperAddr)

	// Read poolIdToTotalFeePpm[poolId] from slot 3
	// mapping slot = keccak256(key ++ slot)
	slotPadded := common.LeftPadBytes(big.NewInt(3).Bytes(), 32)
	data := make([]byte, 64)
	copy(data[0:32], poolId[:])
	copy(data[32:64], slotPadded)
	mappingSlot := common.BytesToHash(crypto.Keccak256(data))
	poolFeeRaw := reader(feeHelperAddr, mappingSlot)
	poolFeePpm := new(big.Int).SetBytes(poolFeeRaw[:]).Uint64()

	// Read protocolFeeSettings from slot 6
	// Struct packing: recipient(address,160bits) | protocolFeePpm(uint16,16bits) | referralFeePpm(uint16,16bits)
	slot6 := common.BigToHash(big.NewInt(6))
	settingsRaw := reader(feeHelperAddr, slot6)
	settingsVal := new(big.Int).SetBytes(settingsRaw[:])
	// protocolFeePpm is at bits [160:176)
	protocolFeePpm := new(big.Int).Rsh(settingsVal, 160)
	protocolFeePpm.And(protocolFeePpm, big.NewInt(0xFFFF))

	return uint32(poolFeePpm + protocolFeePpm.Uint64())
}

func init() {
	// v4PoolManagerAddr is initialized in v4_u256.go init()
	// v4PoolIds will be populated at runtime by RegisterV4Pool
}
