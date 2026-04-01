package formulas

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/holiman/uint256"
)

// ─── LFJ V2.0 (old Liquidity Book) support ───
//
// V2.0 pools use a different storage layout from V2.1/V2.2:
//   - Slot 6: PairInformation (activeId in lowest 24 bits, reserveX at [24:160], reserveY spans slots 6-7)
//   - Slot 10: feeParameters (V2.0 packing with wider uint16 fields)
//   - Slot 11: bins mapping (uint112|uint112 per bin, 4-slot struct)
//   - Slots 12-14: tree[3] (ALL levels are mappings, unlike V2.1 where level0 is a direct slot)
//
// All V2.0 pools are EIP-1167 minimal proxies delegating to 0x49d11cdcd33c042de9cbddf15282cfbe590e9844.

// lfjV2_0Registry maps pool address -> immutable values for LFJ V2.0 pools.
var lfjV2_0Registry = map[string]LFJV2Immutables{
	"0x761969787f55cc9f6ccae3a46ca9f0108b160934": {BinStep: 1, TokenXIsToken0: false}, // USDt.e/USDt
	"0x1d7a1a79e2b4ef88d2323f3845246d24a3c20f1d": {BinStep: 1, TokenXIsToken0: true},  // USDt/USDC
	"0x855ee438445075f25c18a125ba6607543052a194": {BinStep: 1, TokenXIsToken0: false}, // USDC/DAI.e
	"0xe4e7aaa5a1aab5b55ee44fab2d5dd6fcd80e4d42": {BinStep: 2, TokenXIsToken0: false}, // BTC.b/WBTC.e
	"0x12ef33ed026d6eeb6c1ea90e97401ddf3d45f569": {BinStep: 1, TokenXIsToken0: false}, // USDC/USDC.e
	"0x18332988456c4bd9aba6698ec748b331516f5a14": {BinStep: 1, TokenXIsToken0: true},  // USDC.e/USDC
}

// v20TreeLevel0Slot is the precomputed keccak256(abi.encode(0, 12)) for V2.0 tree[0][0].
// In V2.0, all tree levels are mappings, so level0 needs keccak hashing like levels 1 and 2.
var v20TreeLevel0Slot = keccakMappingSlot(0, 12)

// lfjV2LayoutFastV20 is the fast layout for V2.0 pools.
var lfjV2LayoutFastV20 = lfjV2LayoutFast{
	parametersSlot: 10,
	binsSlot:       11,
	treeLevel0Slot: 12,
	treeLevel1Slot: 13,
	treeLevel2Slot: 14,
}

// isV20Layout returns true if the layout is for a V2.0 pool.
// V2.0 uses binsSlot=11, V2.1 uses 6 or 7.
func isV20Layout(layout *lfjV2LayoutFast) bool {
	return layout.binsSlot == 11
}

// lfjV2DecodeParametersV20 extracts fee parameters from V2.0's feeParameters slot (slot 10).
// V2.0 struct packing (FeeHelper.FeeParameters, lowest bits first):
//
//	[0:16]   binStep
//	[16:32]  baseFactor
//	[32:48]  filterPeriod
//	[48:64]  decayPeriod
//	[64:80]  reductionFactor
//	[80:104] variableFeeControl (uint24)
//	[104:120] protocolShare
//	[120:144] maxVolatilityAccumulated (uint24)
//	[144:168] volatilityAccumulated (uint24)
//	[168:192] volatilityReference (uint24)
//	[192:216] indexRef (uint24)
//	[216:256] time (uint40)
func lfjV2DecodeParametersV20(v *big.Int) (staticParams LFJV2StaticFeeParams, varParams LFJV2VariableFeeParams) {
	// binStep at [0:16] — known from registry, skip
	baseFactor := uint16(new(big.Int).And(new(big.Int).Rsh(v, 16), big.NewInt(0xFFFF)).Uint64())
	filterPeriod := uint16(new(big.Int).And(new(big.Int).Rsh(v, 32), big.NewInt(0xFFFF)).Uint64())
	decayPeriod := uint16(new(big.Int).And(new(big.Int).Rsh(v, 48), big.NewInt(0xFFFF)).Uint64())
	reductionFactor := uint16(new(big.Int).And(new(big.Int).Rsh(v, 64), big.NewInt(0xFFFF)).Uint64())
	varFeeControl := uint32(new(big.Int).And(new(big.Int).Rsh(v, 80), big.NewInt(0xFFFFFF)).Uint64())
	protocolShare := uint16(new(big.Int).And(new(big.Int).Rsh(v, 104), big.NewInt(0xFFFF)).Uint64())
	maxVolAcc := uint32(new(big.Int).And(new(big.Int).Rsh(v, 120), big.NewInt(0xFFFFFF)).Uint64())
	volAcc := uint32(new(big.Int).And(new(big.Int).Rsh(v, 144), big.NewInt(0xFFFFFF)).Uint64())
	volRef := uint32(new(big.Int).And(new(big.Int).Rsh(v, 168), big.NewInt(0xFFFFFF)).Uint64())
	idRef := uint32(new(big.Int).And(new(big.Int).Rsh(v, 192), big.NewInt(0xFFFFFF)).Uint64())
	timeLastUpdate := new(big.Int).And(new(big.Int).Rsh(v, 216), new(big.Int).SetUint64(0xFFFFFFFFFF)).Uint64()

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

// FetchLFJV2StateStorageV20 reads LFJ V2.0 state from storage.
func FetchLFJV2StateStorageV20(read StateReader, poolAddress string, token0, token1 string, imm LFJV2Immutables) (*LFJV2State, error) {
	// Read PairInformation at slot 6 to get activeId and global reserves
	pairInfoVal, err := read(poolAddress, big.NewInt(6))
	if err != nil {
		return nil, fmt.Errorf("read PairInformation: %w", err)
	}
	pairInfoBig := new(big.Int).SetBytes(pairInfoVal[:])
	activeId := uint32(new(big.Int).And(pairInfoBig, big.NewInt(0xFFFFFF)).Uint64())

	// reserveX at bits [24:160] of slot 6 (136 bits)
	mask136 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 136), big.NewInt(1))
	globalReserveX := new(big.Int).And(new(big.Int).Rsh(pairInfoBig, 24), mask136)

	// reserveY is in slot 7, bits [0:136] (Solidity doesn't split fields across slots)
	pairInfoVal2, err := read(poolAddress, big.NewInt(7))
	if err != nil {
		return nil, fmt.Errorf("read PairInformation slot 7: %w", err)
	}
	pairInfo2Big := new(big.Int).SetBytes(pairInfoVal2[:])
	globalReserveY := new(big.Int).And(pairInfo2Big, mask136)

	// Read feeParameters at slot 10
	feeParamVal, err := read(poolAddress, big.NewInt(10))
	if err != nil {
		return nil, fmt.Errorf("read feeParameters: %w", err)
	}
	feeParamBig := new(big.Int).SetBytes(feeParamVal[:])
	staticParams, varParams := lfjV2DecodeParametersV20(feeParamBig)

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
		IsV20:             true,
		PoolAddress:       poolAddress,
		GlobalReserveX:    globalReserveX,
		GlobalReserveY:    globalReserveY,
		Bins:              make(map[uint32]LFJV2BinReserves),
		NextBinsDown:      make(map[uint32]uint32),
		NextBinsUp:        make(map[uint32]uint32),
	}

	return state, nil
}

// lfjV2ReadBinU256V20 reads bin reserves for V2.0 pools.
// V2.0 Bin struct occupies 4 slots: first slot packs reserveX(uint112) in lower 112 bits
// and reserveY(uint112) at bits [112:224].
func lfjV2ReadBinU256V20(read StateReader, poolAddress string, binsSlot uint64, binID uint32) (reserveX, reserveY uint256.Int, err error) {
	slot := keccakMappingSlot(binID, binsSlot)
	val, rerr := read(poolAddress, slot)
	if rerr != nil {
		err = rerr
		return
	}
	var packed uint256.Int
	packed.SetBytes32(val[:])
	mask112 := new(uint256.Int).Sub(new(uint256.Int).Lsh(uint256.NewInt(1), 112), uint256.NewInt(1))
	reserveX.And(&packed, mask112)
	var shifted uint256.Int
	shifted.Rsh(&packed, 112)
	reserveY.And(&shifted, mask112)
	return
}

// lfjV2ReadTreeLevel0 reads the tree level0 bitmap. For V2.1, level0 is a direct storage
// slot. For V2.0, level0 is a mapping (_tree[0][0]), so we use the precomputed keccak slot.
func lfjV2ReadTreeLevel0(read StateReader, poolAddress string, layout *lfjV2LayoutFast) ([32]byte, error) {
	if isV20Layout(layout) {
		return read(poolAddress, v20TreeLevel0Slot)
	}
	return read(poolAddress, big.NewInt(int64(layout.treeLevel0Slot)))
}
