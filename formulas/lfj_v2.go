package formulas

import (
	"fmt"
	"math/big"
	"strings"

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
	// Global reserves from _reserves slot (used for rebasing token surplus)
	GlobalReserveX *big.Int // total reserveX tracked by the pool
	GlobalReserveY *big.Int // total reserveY tracked by the pool
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
//
// For V2.1/V2.2 pools (the only ones in lfjV2Registry), the _parameters slot packs
// activeId at bits 232-255. Valid pools have activeId in (0, 0xFFFFFF). Dead pools
// that have been fully drained may have activeId=0 but retain non-zero fee params
// (baseFactor != 0). When both layouts show activeId=0, we fall back to Layout A
// using baseFactor as a tiebreaker. If baseFactor is also 0 on slot 3, try slot 4.
// If all else fails, default to Layout A (most common, ~74% of pools) with a zero
// _parameters value — the resulting state will have activeId=0 and produce zero output.
func lfjV2DetectLayout(read StateReader, poolAddress string) (*lfjV2Layout, *big.Int, error) {
	// Try Layout A first (slot 3) - most common (~74%)
	val, err := read(poolAddress, lfjV2LayoutA.parametersSlot)
	if err != nil {
		return nil, nil, err
	}
	vBig := new(big.Int).SetBytes(val[:])
	activeIdBig := new(big.Int).And(new(big.Int).Rsh(vBig, 232), big.NewInt(0xFFFFFF))
	activeIdA := uint32(activeIdBig.Uint64())

	if activeIdA > 0 && activeIdA < 0xFFFFFF {
		return &lfjV2LayoutA, vBig, nil
	}

	// Try Layout B (slot 4)
	val, err = read(poolAddress, lfjV2LayoutB.parametersSlot)
	if err != nil {
		return nil, nil, err
	}
	vBigB := new(big.Int).SetBytes(val[:])
	activeIdBig = new(big.Int).And(new(big.Int).Rsh(vBigB, 232), big.NewInt(0xFFFFFF))
	activeIdB := uint32(activeIdBig.Uint64())

	if activeIdB > 0 && activeIdB < 0xFFFFFF {
		return &lfjV2LayoutB, vBigB, nil
	}

	// Both layouts show activeId=0 or 0xFFFFFF — pool may be dead/empty.
	// Use baseFactor (bits 0-15) as a tiebreaker: the slot with non-zero baseFactor
	// is more likely to be the real _parameters slot.
	baseFactorA := uint16(new(big.Int).And(vBig, big.NewInt(0xFFFF)).Uint64())
	baseFactorB := uint16(new(big.Int).And(vBigB, big.NewInt(0xFFFF)).Uint64())
	if baseFactorA != 0 {
		return &lfjV2LayoutA, vBig, nil
	}
	if baseFactorB != 0 {
		return &lfjV2LayoutB, vBigB, nil
	}

	// Both fee params are zero — completely uninitialised or edge case.
	// Default to Layout A; activeId=0 will produce zero output safely.
	return &lfjV2LayoutA, vBig, nil
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

	// Read _reserves slot (immediately after _parameters in storage layout)
	reservesSlot := new(big.Int).Add(layout.parametersSlot, big.NewInt(1))
	reservesVal, err := read(poolAddress, reservesSlot)
	if err != nil {
		return nil, nil, fmt.Errorf("read _reserves: %w", err)
	}
	reservesPacked := new(big.Int).SetBytes(reservesVal[:])
	globalReserveX := new(big.Int).And(reservesPacked, mask128)
	globalReserveY := new(big.Int).Rsh(reservesPacked, 128)

	state := &LFJV2State{
		ActiveID:          activeId,
		BinStep:           imm.BinStep,
		TokenX:            tokenX,
		StaticFeeParams:   staticParams,
		VariableFeeParams: varParams,
		IsV20:             false,
		PoolAddress:       poolAddress,
		GlobalReserveX:    globalReserveX,
		GlobalReserveY:    globalReserveY,
		Bins:              make(map[uint32]LFJV2BinReserves),
		NextBinsDown:      make(map[uint32]uint32),
		NextBinsUp:        make(map[uint32]uint32),
	}

	return state, layout, nil
}

// ─── Main Quote Function (legacy eth_call version) ───

// QuoteLFJV2 computes the swap output for an LFJ V2 pool.
// Bins and next-bin lookups are fetched on demand from the RPC pool and cached in state.
// swapForY: true if swapping token X → token Y (zeroForOne).
// blockTimestamp: the block.timestamp used for volatility reference updates.
