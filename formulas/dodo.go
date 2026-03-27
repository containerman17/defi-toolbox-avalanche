package formulas

import (
	"fmt"
	"math/big"
	"strings"
)

// DODO V2 Proactive Market Maker formula.
//
// The PMM algorithm uses an oracle price (i), slippage coefficient (K), and
// equilibrium targets (B0, Q0) to price swaps. The curve is:
//   P = i * (1 - K + K * (B0/B)^2)
//
// State is read via direct storage reads (eth_getStorageAt, cost 0.01 each).
// All pools on Avalanche are EIP-1167 minimal proxies.
//
// Two storage layout families:
//
//   DVM (DODO Vending Machine) — 20 pools, impls 0xacf0cc/0x70efb3:
//     slot 1: _BASE_TOKEN_ (address)
//     slot 3: packed reserves (_BASE_RESERVE_ uint112 | _QUOTE_RESERVE_ uint112)
//     slot 5: "DLP" string (Solidity string storage)
//     slot 13: _LP_FEE_RATE_ (uint256)
//     slot 15: _K_ (uint256)
//     slot 16: _I_ (uint256)
//     R always = ABOVE_ONE, targets computed via adjustedTarget.
//     5 storage reads per pool.
//
//   DSP (DODO Stable Pool) — 15 pools, impls 0x89ba40/0xa7b9c3/0x97d52e/0x77106d:
//     slot 3: _BASE_TOKEN_ (address)
//     slot 5: packed reserves (_BASE_RESERVE_ uint112 | _QUOTE_RESERVE_ uint112)
//     slot 6: packed targets+R (_BASE_TARGET_ uint112 | _QUOTE_TARGET_ uint112 | _RState_ uint32)
//     slot 8: packed fees (_MT_FEE_RATE_MODEL_ address | _LP_FEE_RATE_ uint64)
//     slot 9: packed params (_K_ uint64 | _I_ uint128)
//     5 storage reads per pool.
//
//   DPP-like — 1 pool (impl 0xa952f8):
//     slot 1: _BASE_TOKEN_ (address)
//     slot 3: packed reserves (same as DVM)
//     slot 5: packed targets+R (same as DSP slot 6)
//     slot 15: _LP_FEE_RATE_ (uint256)
//     slot 16: _K_ (uint256)
//     slot 17: _I_ (uint256)
//     6 storage reads per pool.
//
// Detection: slot 5 first byte = 0x44 ('D' from "DLP") → DVM.
// Otherwise DSP or DPP-like (distinguished by slot 8 content).
//
// mtFeeRate computation depends on pool version (checked by FeeRateDIP3Impl):
//   DVM 1.0.2/1.0.3, DSP 1.0.1/1.0.2: mtFeeRate = lpFeeRate * 25 / 100
//   DPP Advanced, DPP 1.0.0: mtFeeRate = 0 (version not recognized by FeeRateDIP3Impl)
// All 36 DODO pools on Avalanche use the same fee model contract.

var (
	dodoONE  = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil) // 1e18
	dodoONE2 = new(big.Int).Exp(big.NewInt(10), big.NewInt(36), nil) // 1e36
)

// RState represents the DODO R equilibrium state.
type RState int

const (
	RStateOne      RState = 0
	RStateAboveOne RState = 1
	RStateBelowOne RState = 2
)

// DODOState holds the PMM state for a DODO V2 pool.
type DODOState struct {
	I         *big.Int // oracle price (1e18 fixed-point)
	K         *big.Int // slippage coefficient (1e18 fixed-point)
	B         *big.Int // base reserve
	Q         *big.Int // quote reserve
	B0        *big.Int // base target (adjusted)
	Q0        *big.Int // quote target (adjusted)
	R         RState   // equilibrium state
	LpFeeRate *big.Int // LP fee rate (1e18 fixed-point)
	MtFeeRate *big.Int // maintainer fee rate (1e18 fixed-point)
	BaseToken string   // base token address
}

// FetchDODOState reads pool state via direct storage reads (eth_getStorageAt).
// 5-6 storage reads per pool (cost 0.05-0.06) instead of 4-5 eth_call (cost 4-5).
func FetchDODOState(reader StateReader, poolAddress, _, _ string) (*DODOState, error) {
	// Read slot 5 to detect pool type.
	// DVM pools store a Solidity string "DLP" here: first byte = 0x44 ('D').
	// DSP/DPP pools store packed reserves or packed targets (numeric data, first byte = 0x00).
	slot5, err := reader(poolAddress, big.NewInt(5))
	if err != nil {
		return nil, fmt.Errorf("read slot 5: %w", err)
	}

	if slot5[0] == 0x44 { // 'D' from "DLP" string
		return fetchDODOStateDVM(reader, poolAddress)
	}

	// DSP or DPP-like pool. Read slot 8 to distinguish.
	// DSP: slot 8 = packed(_MT_FEE_RATE_MODEL_ address(160) | _LP_FEE_RATE_ uint64)
	// DPP: slot 8 = Solidity string "DLP_..." (first byte = 0x44)
	slot8, err := reader(poolAddress, big.NewInt(8))
	if err != nil {
		return nil, fmt.Errorf("read slot 8: %w", err)
	}

	if slot8[0] == 0x44 { // 'D' from "DLP_..." string → DPP-like
		return fetchDODOStateDPP(reader, poolAddress, slot5)
	}

	// DSP layout
	return fetchDODOStateDSP(reader, poolAddress, slot5, slot8)
}

// fetchDODOStateDVM reads state for DVM pools.
// Layout: slot 1=baseToken, slot 3=reserves, slot 13=lpFeeRate, slot 15=K, slot 16=I
func fetchDODOStateDVM(reader StateReader, poolAddress string) (*DODOState, error) {
	// Read slot 1: _BASE_TOKEN_
	slot1, err := reader(poolAddress, big.NewInt(1))
	if err != nil {
		return nil, fmt.Errorf("read slot 1: %w", err)
	}
	baseToken := strings.ToLower("0x" + fmt.Sprintf("%x", slot1[12:32]))

	// Read slot 3: packed reserves
	slot3, err := reader(poolAddress, big.NewInt(3))
	if err != nil {
		return nil, fmt.Errorf("read slot 3: %w", err)
	}
	B, Q := unpackReserves(slot3)

	// Read slot 13: _LP_FEE_RATE_
	slot13, err := reader(poolAddress, big.NewInt(13))
	if err != nil {
		return nil, fmt.Errorf("read slot 13: %w", err)
	}
	lpFeeRate := new(big.Int).SetBytes(slot13[:])

	// Read slot 15: _K_
	slot15, err := reader(poolAddress, big.NewInt(15))
	if err != nil {
		return nil, fmt.Errorf("read slot 15: %w", err)
	}
	K := new(big.Int).SetBytes(slot15[:])

	// Read slot 16: _I_
	slot16, err := reader(poolAddress, big.NewInt(16))
	if err != nil {
		return nil, fmt.Errorf("read slot 16: %w", err)
	}
	I := new(big.Int).SetBytes(slot16[:])

	// DVM: R=ABOVE_ONE, B0/Q0 computed by adjustedTarget
	state := &DODOState{
		I:         I,
		K:         K,
		B:         B,
		Q:         Q,
		B0:        big.NewInt(0),
		Q0:        big.NewInt(0),
		R:         RStateAboveOne,
		LpFeeRate: lpFeeRate,
		MtFeeRate: new(big.Int).Div(new(big.Int).Mul(lpFeeRate, big.NewInt(25)), big.NewInt(100)),
		BaseToken: baseToken,
	}

	dodoAdjustedTarget(state)
	return state, nil
}

// fetchDODOStateDSP reads state for DSP pools.
// Layout: slot 3=baseToken, slot 5=reserves(already read), slot 6=targets+R,
//         slot 8=packed(lpFee|model)(already read), slot 9=packed(K|I)
func fetchDODOStateDSP(reader StateReader, poolAddress string, slot5, slot8 [32]byte) (*DODOState, error) {
	// slot 3: _BASE_TOKEN_
	slot3, err := reader(poolAddress, big.NewInt(3))
	if err != nil {
		return nil, fmt.Errorf("read slot 3: %w", err)
	}
	baseToken := strings.ToLower("0x" + fmt.Sprintf("%x", slot3[12:32]))

	// slot 5 already read: packed reserves
	B, Q := unpackReserves(slot5)

	// slot 6: packed targets + R state
	slot6, err := reader(poolAddress, big.NewInt(6))
	if err != nil {
		return nil, fmt.Errorf("read slot 6: %w", err)
	}
	B0, Q0, R := unpackTargetsAndR(slot6)

	// slot 8 already read: packed(_MT_FEE_RATE_MODEL_ address(160) | _LP_FEE_RATE_ uint64)
	// _LP_FEE_RATE_ is uint64 at bits 160-223 (bytes 4-11 from left in big-endian)
	slot8Val := new(big.Int).SetBytes(slot8[:])
	mask64 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 64), big.NewInt(1))
	lpFeeRate := new(big.Int).And(new(big.Int).Rsh(slot8Val, 160), mask64)

	// mtFeeRate for DSP-layout pools:
	// All pools reaching this code path are "DPP Advanced" or "DPP 1.0.0" pools.
	// Their version strings are NOT recognized by FeeRateDIP3Impl
	// (only "DSP 1.0.1"/"DSP 1.0.2"/"DVM 1.0.2"/"DVM 1.0.3" are recognized),
	// so getFeeRate() returns 0. The single actual DSP pool on Avalanche
	// uses the DPP-like detection path (slot8[0]='D'), not this code path.
	mtFeeRate := big.NewInt(0)

	// slot 9: packed(_K_ uint64 | _I_ uint128)
	// K at bits 0-63, I at bits 64-191
	slot9, err := reader(poolAddress, big.NewInt(9))
	if err != nil {
		return nil, fmt.Errorf("read slot 9: %w", err)
	}
	slot9Val := new(big.Int).SetBytes(slot9[:])
	K := new(big.Int).And(slot9Val, mask64)
	mask128 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	I := new(big.Int).And(new(big.Int).Rsh(slot9Val, 64), mask128)

	state := &DODOState{
		I:         I,
		K:         K,
		B:         B,
		Q:         Q,
		B0:        B0,
		Q0:        Q0,
		R:         R,
		LpFeeRate: lpFeeRate,
		MtFeeRate: mtFeeRate,
		BaseToken: baseToken,
	}

	dodoAdjustedTarget(state)
	return state, nil
}

// fetchDODOStateDPP reads state for the DPP-like pool (impl 0xa952f8).
// Layout: slot 1=baseToken, slot 3=reserves, slot 5=targets+R(already read),
//         slot 15=lpFeeRate, slot 16=K, slot 17=I
func fetchDODOStateDPP(reader StateReader, poolAddress string, slot5 [32]byte) (*DODOState, error) {
	// slot 1: _BASE_TOKEN_
	slot1, err := reader(poolAddress, big.NewInt(1))
	if err != nil {
		return nil, fmt.Errorf("read slot 1: %w", err)
	}
	baseToken := strings.ToLower("0x" + fmt.Sprintf("%x", slot1[12:32]))

	// slot 3: packed reserves
	slot3, err := reader(poolAddress, big.NewInt(3))
	if err != nil {
		return nil, fmt.Errorf("read slot 3: %w", err)
	}
	B, Q := unpackReserves(slot3)

	// slot 5 already read: packed targets + R
	B0, Q0, R := unpackTargetsAndR(slot5)

	// slot 15: _LP_FEE_RATE_
	slot15, err := reader(poolAddress, big.NewInt(15))
	if err != nil {
		return nil, fmt.Errorf("read slot 15: %w", err)
	}
	lpFeeRate := new(big.Int).SetBytes(slot15[:])

	// slot 16: _K_
	slot16, err := reader(poolAddress, big.NewInt(16))
	if err != nil {
		return nil, fmt.Errorf("read slot 16: %w", err)
	}
	K := new(big.Int).SetBytes(slot16[:])

	// slot 17: _I_
	slot17, err := reader(poolAddress, big.NewInt(17))
	if err != nil {
		return nil, fmt.Errorf("read slot 17: %w", err)
	}
	I := new(big.Int).SetBytes(slot17[:])

	state := &DODOState{
		I:         I,
		K:         K,
		B:         B,
		Q:         Q,
		B0:        B0,
		Q0:        Q0,
		R:         R,
		LpFeeRate: lpFeeRate,
		MtFeeRate: new(big.Int).Div(new(big.Int).Mul(lpFeeRate, big.NewInt(25)), big.NewInt(100)),
		BaseToken: baseToken,
	}

	dodoAdjustedTarget(state)
	return state, nil
}

// unpackReserves extracts B and Q from a packed storage slot.
// Layout: lower 112 bits = _BASE_RESERVE_, next 112 bits = _QUOTE_RESERVE_
func unpackReserves(slot [32]byte) (*big.Int, *big.Int) {
	val := new(big.Int).SetBytes(slot[:])
	mask112 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 112), big.NewInt(1))
	B := new(big.Int).And(val, mask112)
	Q := new(big.Int).And(new(big.Int).Rsh(val, 112), mask112)
	return B, Q
}

// unpackTargetsAndR extracts B0, Q0, and R from a packed storage slot.
// Layout: lower 112 bits = _BASE_TARGET_, next 112 bits = _QUOTE_TARGET_, top bits = _RState_
func unpackTargetsAndR(slot [32]byte) (*big.Int, *big.Int, RState) {
	val := new(big.Int).SetBytes(slot[:])
	mask112 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 112), big.NewInt(1))
	B0 := new(big.Int).And(val, mask112)
	Q0 := new(big.Int).And(new(big.Int).Rsh(val, 112), mask112)
	R := RState(new(big.Int).Rsh(val, 224).Int64())
	return B0, Q0, R
}

// dodoAdjustedTarget re-computes the non-primary target after reading raw storage.
// This matches PMMPricing.adjustedTarget() in Solidity.
func dodoAdjustedTarget(state *DODOState) {
	if state.R == RStateBelowOne {
		// Q0 = _SolveQuadraticFunctionForTarget(Q, B-B0, I, K)
		delta := new(big.Int).Sub(state.B, state.B0)
		state.Q0 = dodoSolveQuadraticForTarget(state.Q, delta, state.I, state.K)
	} else if state.R == RStateAboveOne {
		// B0 = _SolveQuadraticFunctionForTarget(B, Q-Q0, reciprocalFloor(I), K)
		delta := new(big.Int).Sub(state.Q, state.Q0)
		state.B0 = dodoSolveQuadraticForTarget(state.B, delta, dodoReciprocalFloor(state.I), state.K)
	}
}

// dodoSolveQuadraticForTarget implements DODOMath._SolveQuadraticFunctionForTarget.
// Given V1, delta, i, k: returns V0 such that the integral from V0 to V1 equals delta*i.
// Formula: V0 = V1 * (1 + (sqrt(1 + 4*k*i*delta/V1) - 1) / (2*k))
func dodoSolveQuadraticForTarget(V1, delta, i, k *big.Int) *big.Int {
	if k.Sign() == 0 {
		// k == 0: V0 = V1 + i*delta/1e18
		return new(big.Int).Add(V1, dodoMulFloor(i, delta))
	}
	if V1.Sign() == 0 {
		return big.NewInt(0)
	}

	// sqrt = sqrt(1 + 4*k*i*delta/V1) in 1e36 precision
	// ki = 4 * k * i
	ki := new(big.Int).Mul(big.NewInt(4), k)
	ki.Mul(ki, i)

	var sqrtVal *big.Int
	if ki.Sign() == 0 {
		sqrtVal = new(big.Int).Set(dodoONE)
	} else {
		// Check overflow: if ki*delta doesn't overflow (Go big.Int never overflows)
		kiDelta := new(big.Int).Mul(ki, delta)
		// sqrt = sqrt(ki*delta/V1 + 1e36)
		inner := new(big.Int).Div(kiDelta, V1)
		inner.Add(inner, dodoONE2)
		sqrtVal = dodoSqrt(inner)
	}

	// premium = divFloor(sqrt - ONE, k * 2) + ONE
	// divFloor(a, b) = a * 1e18 / b
	numerator := new(big.Int).Sub(sqrtVal, dodoONE)
	k2 := new(big.Int).Mul(k, big.NewInt(2))
	premium := dodoMulFloor1e18DivFloor(numerator, k2)
	premium.Add(premium, dodoONE)

	// V0 = V1 * premium / 1e18
	return dodoMulFloor(V1, premium)
}

// QuoteDODO computes the output amount for a DODO V2 swap.
// sellBase: true if selling base token (base -> quote), false if selling quote (quote -> base).
func QuoteDODO(state *DODOState, amountIn *big.Int, sellBase bool) *big.Int {
	var receiveAmount *big.Int

	if sellBase {
		receiveAmount = dodoSellBaseToken(state, amountIn)
	} else {
		receiveAmount = dodoSellQuoteToken(state, amountIn)
	}

	// Apply fees on original receiveAmount (parallel, matching Solidity).
	// Solidity: lpFee = receiveAmount * lpFeeRate / 1e18
	//           mtFee = receiveAmount * mtFeeRate / 1e18
	//           receiveAmount -= (lpFee + mtFee)
	lpFee := dodoMulFloor(receiveAmount, state.LpFeeRate)
	totalFee := new(big.Int).Set(lpFee)
	if state.MtFeeRate != nil && state.MtFeeRate.Sign() > 0 {
		mtFee := dodoMulFloor(receiveAmount, state.MtFeeRate)
		totalFee.Add(totalFee, mtFee)
	}
	receiveAmount = new(big.Int).Sub(receiveAmount, totalFee)

	return receiveAmount
}

// ============ PMM Pricing ============

func dodoSellBaseToken(state *DODOState, payBaseAmount *big.Int) *big.Int {
	switch state.R {
	case RStateOne:
		// R=1: falls below one
		return dodoROneSellBaseToken(state, payBaseAmount)

	case RStateAboveOne:
		// R>1: complex case
		backToOnePayBase := new(big.Int).Sub(state.B0, state.B)
		backToOneReceiveQuote := new(big.Int).Sub(state.Q, state.Q0)

		if payBaseAmount.Cmp(backToOnePayBase) < 0 {
			// case 2.1: R stays above one
			result := dodoRAboveSellBaseToken(state, payBaseAmount)
			if result.Cmp(backToOneReceiveQuote) > 0 {
				result = new(big.Int).Set(backToOneReceiveQuote)
			}
			return result
		} else if payBaseAmount.Cmp(backToOnePayBase) == 0 {
			// case 2.2: R goes to ONE
			return new(big.Int).Set(backToOneReceiveQuote)
		} else {
			// case 2.3: R goes below one
			remainder := new(big.Int).Sub(payBaseAmount, backToOnePayBase)
			r := dodoROneSellBaseToken(state, remainder)
			if r == nil {
				return nil // on-chain revert
			}
			return new(big.Int).Add(backToOneReceiveQuote, r)
		}

	default:
		// R<1
		return dodoRBelowSellBaseToken(state, payBaseAmount)
	}
}

func dodoSellQuoteToken(state *DODOState, payQuoteAmount *big.Int) *big.Int {
	switch state.R {
	case RStateOne:
		return dodoROneSellQuoteToken(state, payQuoteAmount)

	case RStateAboveOne:
		return dodoRAboveSellQuoteToken(state, payQuoteAmount)

	default:
		// R<1: complex case (mirror of sellBase R>1)
		backToOnePayQuote := new(big.Int).Sub(state.Q0, state.Q)
		backToOneReceiveBase := new(big.Int).Sub(state.B, state.B0)

		if payQuoteAmount.Cmp(backToOnePayQuote) < 0 {
			result := dodoRBelowSellQuoteToken(state, payQuoteAmount)
			if result.Cmp(backToOneReceiveBase) > 0 {
				result = new(big.Int).Set(backToOneReceiveBase)
			}
			return result
		} else if payQuoteAmount.Cmp(backToOnePayQuote) == 0 {
			return new(big.Int).Set(backToOneReceiveBase)
		} else {
			remainder := new(big.Int).Sub(payQuoteAmount, backToOnePayQuote)
			r := dodoROneSellQuoteToken(state, remainder)
			if r == nil {
				return nil // on-chain revert
			}
			return new(big.Int).Add(backToOneReceiveBase, r)
		}
	}
}

// ============ R = 1 cases ============

func dodoROneSellBaseToken(state *DODOState, payBaseAmount *big.Int) *big.Int {
	return dodoSolveQuadraticForTrade(state.Q0, state.Q0, payBaseAmount, state.I, state.K)
}

func dodoROneSellQuoteToken(state *DODOState, payQuoteAmount *big.Int) *big.Int {
	return dodoSolveQuadraticForTrade(state.B0, state.B0, payQuoteAmount, dodoReciprocalFloor(state.I), state.K)
}

// ============ R < 1 cases ============

func dodoRBelowSellBaseToken(state *DODOState, payBaseAmount *big.Int) *big.Int {
	return dodoSolveQuadraticForTrade(state.Q0, state.Q, payBaseAmount, state.I, state.K)
}

func dodoRBelowSellQuoteToken(state *DODOState, payQuoteAmount *big.Int) *big.Int {
	qPlusPay := new(big.Int).Add(state.Q, payQuoteAmount)
	return dodoGeneralIntegrate(state.Q0, qPlusPay, state.Q, dodoReciprocalFloor(state.I), state.K)
}

// ============ R > 1 cases ============

func dodoRAboveSellBaseToken(state *DODOState, payBaseAmount *big.Int) *big.Int {
	bPlusPay := new(big.Int).Add(state.B, payBaseAmount)
	return dodoGeneralIntegrate(state.B0, bPlusPay, state.B, state.I, state.K)
}

func dodoRAboveSellQuoteToken(state *DODOState, payQuoteAmount *big.Int) *big.Int {
	return dodoSolveQuadraticForTrade(state.B0, state.B, payQuoteAmount, dodoReciprocalFloor(state.I), state.K)
}

// ============ DODOMath ============

// dodoGeneralIntegrate computes: i*delta*(1-k+k*(V0^2/V1/V2))
// where delta = V1 - V2. Requires V0 >= V1 >= V2 > 0.
// Rounds down.
func dodoGeneralIntegrate(V0, V1, V2, i, k *big.Int) *big.Int {
	if V0.Sign() == 0 {
		panic("DODO: TARGET_IS_ZERO")
	}
	// fairAmount = i * (V1 - V2)
	delta := new(big.Int).Sub(V1, V2)
	fairAmount := new(big.Int).Mul(i, delta)

	if k.Sign() == 0 {
		// k == 0: return fairAmount / 1e18
		return new(big.Int).Div(fairAmount, dodoONE)
	}

	// V0V0V1V2 = divFloor(V0*V0/V1, V2) = (V0*V0/V1 * 1e18) / V2
	v0v0 := new(big.Int).Mul(V0, V0)
	v0v0divV1 := new(big.Int).Div(v0v0, V1)
	V0V0V1V2 := dodoMulFloor1e18DivFloor(v0v0divV1, V2) // divFloor(v0v0divV1, V2) = v0v0divV1 * 1e18 / V2

	// penalty = mulFloor(k, V0V0V1V2) = k * V0V0V1V2 / 1e18
	penalty := dodoMulFloor(k, V0V0V1V2)

	// result = (1e18 - k + penalty) * fairAmount / 1e36
	bracket := new(big.Int).Sub(dodoONE, k)
	bracket.Add(bracket, penalty)
	result := new(big.Int).Mul(bracket, fairAmount)
	result.Div(result, dodoONE2)

	return result
}

// dodoSolveQuadraticForTrade solves for the trade amount.
// Given V0 (target), V1 (current), delta (input * price), i, k:
// Returns |V1 - V2| where V2 is the new balance.
func dodoSolveQuadraticForTrade(V0, V1, delta, i, k *big.Int) *big.Int {
	if V0.Sign() == 0 {
		panic("DODO: TARGET_IS_ZERO")
	}
	if delta.Sign() == 0 {
		return big.NewInt(0)
	}

	if k.Sign() == 0 {
		// k == 0: return min(i*delta/1e18, V1)
		result := dodoMulFloor(i, delta)
		if result.Cmp(V1) > 0 {
			return new(big.Int).Set(V1)
		}
		return result
	}

	if k.Cmp(dodoONE) == 0 {
		// k == 1: special case
		// temp = i*delta*V1 / (V0*V0)
		idelta := new(big.Int).Mul(i, delta)
		var temp *big.Int
		if idelta.Sign() == 0 {
			temp = big.NewInt(0)
		} else {
			// Check for overflow: if idelta*V1 doesn't overflow
			ideltaV1 := new(big.Int).Mul(idelta, V1)
			checkDiv := new(big.Int).Div(ideltaV1, idelta)
			v0v0 := new(big.Int).Mul(V0, V0)
			if checkDiv.Cmp(V1) == 0 {
				temp = new(big.Int).Div(ideltaV1, v0v0)
			} else {
				// Overflow path: delta*V1/V0 * i/V0
				temp = new(big.Int).Mul(delta, V1)
				temp.Div(temp, V0)
				temp.Mul(temp, i)
				temp.Div(temp, V0)
			}
		}
		// return V1 * temp / (temp + 1e18)
		num := new(big.Int).Mul(V1, temp)
		den := new(big.Int).Add(temp, dodoONE)
		return new(big.Int).Div(num, den)
	}

	// General case: solve quadratic
	// part2 = k*V0/V1*V0 + i*delta  (= kQ0^2/Q1 + i*deltaB)
	kV0 := new(big.Int).Mul(k, V0)
	kV0divV1 := new(big.Int).Div(kV0, V1)
	kV0divV1timesV0 := new(big.Int).Mul(kV0divV1, V0)
	idelta := new(big.Int).Mul(i, delta)
	part2 := new(big.Int).Add(kV0divV1timesV0, idelta)

	// bAbs = (1e18 - k) * V1  (= (1-k)*Q1)
	oneMinusK := new(big.Int).Sub(dodoONE, k)
	bAbs := new(big.Int).Mul(oneMinusK, V1)

	var bSig bool
	if bAbs.Cmp(part2) >= 0 {
		bAbs = new(big.Int).Sub(bAbs, part2)
		bSig = false
	} else {
		bAbs = new(big.Int).Sub(part2, bAbs)
		bSig = true
	}
	bAbs.Div(bAbs, dodoONE)

	// squareRoot = sqrt(bAbs^2 + 4*(1-k)*k*V0*V0 / 1e18)
	// First compute 4*(1-k)*k*V0^2 using mulFloor
	fourOneMinusK := new(big.Int).Mul(oneMinusK, big.NewInt(4))
	kV0Sq := dodoMulFloor(k, V0)
	kV0Sq.Mul(kV0Sq, V0)
	inner := dodoMulFloor(fourOneMinusK, kV0Sq) // 4*(1-k) * k*V0*V0/1e18

	bAbsSq := new(big.Int).Mul(bAbs, bAbs)
	sqrtInput := new(big.Int).Add(bAbsSq, inner)
	squareRoot := dodoSqrt(sqrtInput)

	// denominator = 2*(1-k)
	denominator := new(big.Int).Mul(oneMinusK, big.NewInt(2))

	var numerator *big.Int
	if bSig {
		numerator = new(big.Int).Sub(squareRoot, bAbs)
		if numerator.Sign() <= 0 {
			// On-chain: require(numerator > 0) → revert.
			// Swap is impossible at this size.
			return nil
		}
	} else {
		numerator = new(big.Int).Add(bAbs, squareRoot)
	}

	// V2 = divCeil(numerator, denominator) = ceil(numerator * 1e18 / denominator)
	V2 := dodoDivCeil(numerator, denominator)

	if V2.Cmp(V1) > 0 {
		return big.NewInt(0)
	}
	return new(big.Int).Sub(V1, V2)
}

// ============ DecimalMath helpers ============

// dodoMulFloor: target * d / 1e18
func dodoMulFloor(target, d *big.Int) *big.Int {
	result := new(big.Int).Mul(target, d)
	return result.Div(result, dodoONE)
}

// dodoMulFloor1e18DivFloor: target * 1e18 / d (= divFloor)
func dodoMulFloor1e18DivFloor(target, d *big.Int) *big.Int {
	result := new(big.Int).Mul(target, dodoONE)
	return result.Div(result, d)
}

// dodoDivCeil: ceil(target * 1e18 / d)
func dodoDivCeil(target, d *big.Int) *big.Int {
	num := new(big.Int).Mul(target, dodoONE)
	result := new(big.Int).Div(num, d)
	rem := new(big.Int).Mod(num, d)
	if rem.Sign() > 0 {
		result.Add(result, big.NewInt(1))
	}
	return result
}

// dodoReciprocalFloor: 1e36 / target
func dodoReciprocalFloor(target *big.Int) *big.Int {
	return new(big.Int).Div(dodoONE2, target)
}

// dodoSqrt implements the same Babylonian method as Solidity's SafeMath.sqrt.
func dodoSqrt(x *big.Int) *big.Int {
	if x.Sign() == 0 {
		return big.NewInt(0)
	}
	// z = x/2 + 1
	z := new(big.Int).Div(x, big.NewInt(2))
	z.Add(z, big.NewInt(1))
	y := new(big.Int).Set(x)
	for z.Cmp(y) < 0 {
		y.Set(z)
		// z = (x/z + z) / 2
		xDivZ := new(big.Int).Div(x, z)
		z.Add(xDivZ, z)
		z.Div(z, big.NewInt(2))
	}
	return y
}
