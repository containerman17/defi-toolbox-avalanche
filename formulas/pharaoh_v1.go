package formulas

import (
	"fmt"
	"math/big"
	"github.com/ava-labs/libevm/crypto"


)

// Pharaoh V1 is a Solidly-fork AMM with two curve types:
// - Volatile: x * y = k (constant product, same as V2 but fee applied differently)
// - Stable:   x³y + y³x = k (StableSwap-like curve)
//
// The contract applies fee as: adjustedIn = amountIn - (amountIn * fee / 10000)
// then computes the output using the appropriate curve.
//
// State is read via metadata() which returns (decimals0, decimals1, reserve0, reserve1, stable, token0, token1).
// Fee is determined by calling getAmountOut() once with a test amount and reverse-engineering.

type PharaohV1State struct {
	Decimals0   *big.Int // 10^(token0 decimals), e.g. 1e6 for USDC
	Decimals1   *big.Int
	Reserve0    *big.Int
	Reserve1    *big.Int
	Stable      bool
	Token0      string
	Token1      string
	FeeBps      int  // fee in basis points (1-10000)
	SubtractOne bool // some Solidly forks do `return amountOut - 1`
}

var metadataSelector = crypto.Keccak256([]byte("metadata()"))[:4]
var getAmountOutSelector = crypto.Keccak256([]byte("getAmountOut(uint256,address)"))[:4]
var factorySelector = crypto.Keccak256([]byte("factory()"))[:4]
var getFeeSelector = crypto.Keccak256([]byte("getFee(bool)"))[:4]       // 0x512b45ea
var getFeesSelector = crypto.Keccak256([]byte("getFees(bool)"))[:4]     // 0x14ca639e
var pairFeeSelector = crypto.Keccak256([]byte("pairFee(address)"))[:4]  // per-pool fee override
var poolFeeSelector = crypto.Keccak256([]byte("fee()"))[:4]             // fee stored on pool

// proxyImplSlot is the storage slot used by Pharaoh proxy pairs to store the
// factory/resolver address. This is NOT the standard EIP-1967 slot — it's a
// custom slot used by Pharaoh's TransparentUpgradeableProxy.
const proxyImplSlot = "0xa3f0ad74e5423aebfd80d3ef4346578335a9a72aeaee59ff6cb3582b35133d50"

// FetchPharaohV1State reads pool state via metadata() and determines the fee
// by reading it from the factory contract. Falls back to getAmountOut() probing
// if factory fee lookup fails. 2-4 EVM calls total.

// readFeeFromFactory reads the fee from the pool's factory contract.
// It first tries factory() on the pool to get the factory address. If that
// fails (proxy pools), it reads the factory from a known proxy storage slot.
//
// Tries (in order):
// 1. pairFee(pool) on factory for per-pool fee overrides
// 2. getFee(bool) or getFees(bool) on factory for global defaults
// 3. fee() on the pool contract itself

// readFeeFromPool reads the fee directly from the pool contract via fee().
// This is used as a last resort when factory methods and getAmountOut probing both fail.
// Returns (fee in bps, error).

// getFactoryAddressEx returns the factory address for a Pharaoh V1 pool,
// along with a flag indicating whether the factory was discovered via the
// proxy storage slot (vs. factory() call). This matters because proxy factories
// support pairFee(address) for per-pool fee overrides.

// detectSubtractOne calls getAmountOut with a test amount and checks if the
// formula output minus 1 matches the on-chain result. Returns false if the
// pool's reserves are too imbalanced for a meaningful test.

// detectPharaohV1Fee calls getAmountOut once and reverse-engineers the fee.
// Returns (fee, subtractOne, error).

// QuotePharaohV1 computes the output amount for a Pharaoh V1 swap.
// zeroForOne: true if swapping token0 -> token1.
func QuotePharaohV1(state *PharaohV1State, amountIn *big.Int, zeroForOne bool) *big.Int {
	out := quotePharaohV1Internal(state, amountIn, zeroForOne, state.FeeBps)
	if state.SubtractOne && out.Sign() > 0 {
		out.Sub(out, big.NewInt(1))
	}
	return out
}

func quotePharaohV1Internal(state *PharaohV1State, amountIn *big.Int, zeroForOne bool, feeBps int) *big.Int {
	// Apply fee: adjustedIn = amountIn - (amountIn * feeBps / 10000)
	fee := new(big.Int).Mul(amountIn, big.NewInt(int64(feeBps)))
	fee.Div(fee, big.NewInt(10000))
	adjustedIn := new(big.Int).Sub(amountIn, fee)

	return getAmountOut(state, adjustedIn, zeroForOne)
}

// getAmountOut implements the core pricing logic (after fee deduction).
func getAmountOut(state *PharaohV1State, amountIn *big.Int, zeroForOne bool) *big.Int {
	if state.Stable {
		return getAmountOutStable(state, amountIn, zeroForOne)
	}
	return getAmountOutVolatile(state, amountIn, zeroForOne)
}

// getAmountOutVolatile: amountOut = adjustedIn * reserveB / (reserveA + adjustedIn)
func getAmountOutVolatile(state *PharaohV1State, amountIn *big.Int, zeroForOne bool) *big.Int {
	var reserveA, reserveB *big.Int
	if zeroForOne {
		reserveA = state.Reserve0
		reserveB = state.Reserve1
	} else {
		reserveA = state.Reserve1
		reserveB = state.Reserve0
	}

	// amountOut = amountIn * reserveB / (reserveA + amountIn)
	num := new(big.Int).Mul(amountIn, reserveB)
	den := new(big.Int).Add(reserveA, amountIn)
	return new(big.Int).Div(num, den)
}

// getAmountOutStable implements the x³y + y³x = k stable curve.
// This mirrors the Solidity _getAmountOut for stable pairs.
func getAmountOutStable(state *PharaohV1State, amountIn *big.Int, zeroForOne bool) *big.Int {
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

	// Compute invariant k from raw reserves
	xy := kStable(state.Reserve0, state.Reserve1, state.Decimals0, state.Decimals1, e18)

	// Normalize reserves to 18 decimals
	r0Norm := new(big.Int).Mul(state.Reserve0, e18)
	r0Norm.Div(r0Norm, state.Decimals0)
	r1Norm := new(big.Int).Mul(state.Reserve1, e18)
	r1Norm.Div(r1Norm, state.Decimals1)

	var reserveA, reserveB *big.Int
	var decimalsIn, decimalsOut *big.Int
	if zeroForOne {
		reserveA = r0Norm
		reserveB = r1Norm
		decimalsIn = state.Decimals0
		decimalsOut = state.Decimals1
	} else {
		reserveA = r1Norm
		reserveB = r0Norm
		decimalsIn = state.Decimals1
		decimalsOut = state.Decimals0
	}

	// Normalize amountIn to 18 decimals
	amountInNorm := new(big.Int).Mul(amountIn, e18)
	amountInNorm.Div(amountInNorm, decimalsIn)

	// y = reserveB - get_y(amountInNorm + reserveA, xy, reserveB)
	xNew := new(big.Int).Add(amountInNorm, reserveA)
	yNew := getY(xNew, xy, reserveB, e18)
	dy := new(big.Int).Sub(reserveB, yNew)

	// De-normalize to output token decimals
	dy.Mul(dy, decimalsOut)
	dy.Div(dy, e18)

	return dy
}

// kStable computes the invariant: k = (x * y / 1e18) * (x² / 1e18 + y² / 1e18) / 1e18
// where x, y are normalized to 18 decimals.
func kStable(reserve0, reserve1, decimals0, decimals1, e18 *big.Int) *big.Int {
	x := new(big.Int).Mul(reserve0, e18)
	x.Div(x, decimals0)
	y := new(big.Int).Mul(reserve1, e18)
	y.Div(y, decimals1)

	// _a = (x * y) / 1e18
	a := new(big.Int).Mul(x, y)
	a.Div(a, e18)

	// _b = (x*x/1e18 + y*y/1e18)
	xx := new(big.Int).Mul(x, x)
	xx.Div(xx, e18)
	yy := new(big.Int).Mul(y, y)
	yy.Div(yy, e18)
	b := new(big.Int).Add(xx, yy)

	// k = (a * b) / 1e18
	k := new(big.Int).Mul(a, b)
	k.Div(k, e18)
	return k
}

// f computes: f(x0, y) = (x0 * y / 1e18) * (x0² / 1e18 + y² / 1e18) / 1e18
func f(x0, y, e18 *big.Int) *big.Int {
	a := new(big.Int).Mul(x0, y)
	a.Div(a, e18)

	xx := new(big.Int).Mul(x0, x0)
	xx.Div(xx, e18)
	yy := new(big.Int).Mul(y, y)
	yy.Div(yy, e18)
	b := new(big.Int).Add(xx, yy)

	result := new(big.Int).Mul(a, b)
	result.Div(result, e18)
	return result
}

// d computes the derivative: d(x0, y) = 3 * x0 * (y² / 1e18) / 1e18 + (x0² / 1e18 * x0) / 1e18
func d(x0, y, e18 *big.Int) *big.Int {
	// 3 * x0 * (y*y/1e18) / 1e18
	yy := new(big.Int).Mul(y, y)
	yy.Div(yy, e18)
	term1 := new(big.Int).Mul(big.NewInt(3), x0)
	term1.Mul(term1, yy)
	term1.Div(term1, e18)

	// ((x0*x0/1e18) * x0) / 1e18
	xx := new(big.Int).Mul(x0, x0)
	xx.Div(xx, e18)
	term2 := new(big.Int).Mul(xx, x0)
	term2.Div(term2, e18)

	return new(big.Int).Add(term1, term2)
}

// FetchPharaohV1StateStorage reads pool state using storage reads instead of eth_call.
// Uses the hardcoded registry for immutable data (fee, decimals, stable, subtractOne)
// and reads only the reserves via StateReader. Returns nil, nil if pool is not in registry.
func FetchPharaohV1StateStorage(reader StateReader, poolAddress string) (*PharaohV1State, error) {
	cfg, ok := pharaohV1Registry[poolAddress]
	if !ok {
		return nil, nil // not in registry, caller should fall back
	}

	state := &PharaohV1State{
		Decimals0:   new(big.Int).SetUint64(cfg.Decimals0),
		Decimals1:   new(big.Int).SetUint64(cfg.Decimals1),
		Stable:      cfg.Stable,
		FeeBps:      cfg.Fee,
		SubtractOne: cfg.SubtractOne,
	}

	if cfg.PackedSlot >= 0 {
		// Packed layout: ts(32)|r1(112)|r0(112) in one slot
		data, err := reader(poolAddress, big.NewInt(int64(cfg.PackedSlot)))
		if err != nil {
			return nil, fmt.Errorf("read packed slot %d: %w", cfg.PackedSlot, err)
		}
		// r0 = lower 112 bits (bytes 18..31), r1 = next 112 bits (bytes 4..17)
		mask112 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 112), big.NewInt(1))
		val := new(big.Int).SetBytes(data[:])
		state.Reserve0 = new(big.Int).And(val, mask112)
		state.Reserve1 = new(big.Int).And(new(big.Int).Rsh(val, 112), mask112)

		// Fee is mutable on-chain; read it from slot 16 (per-million) and convert to bps
		feeData, err := reader(poolAddress, big.NewInt(16))
		if err == nil {
			feePerMillion := new(big.Int).SetBytes(feeData[:])
			if feePerMillion.Sign() > 0 {
				state.FeeBps = int(feePerMillion.Int64() / 100)
			}
		}
	} else {
		// Separate reserve slots
		r0data, err := reader(poolAddress, big.NewInt(int64(cfg.Reserve0Slot)))
		if err != nil {
			return nil, fmt.Errorf("read reserve0 slot %d: %w", cfg.Reserve0Slot, err)
		}
		r1data, err := reader(poolAddress, big.NewInt(int64(cfg.Reserve1Slot)))
		if err != nil {
			return nil, fmt.Errorf("read reserve1 slot %d: %w", cfg.Reserve1Slot, err)
		}
		state.Reserve0 = new(big.Int).SetBytes(r0data[:])
		state.Reserve1 = new(big.Int).SetBytes(r1data[:])
	}

	return state, nil
}

// getY implements Newton-Raphson iteration to find y such that f(x0, y) = xy (the invariant).
// This mirrors the Solidity _get_y function exactly.
func getY(x0, xy, y0, e18 *big.Int) *big.Int {
	y := new(big.Int).Set(y0)

	for i := 0; i < 255; i++ {
		yPrev := new(big.Int).Set(y)
		k := f(x0, y, e18)

		if k.Cmp(xy) < 0 {
			diff := new(big.Int).Sub(xy, k)
			dy := new(big.Int).Mul(diff, e18)
			dVal := d(x0, y, e18)
			if dVal.Sign() == 0 {
				return y
			}
			dy.Div(dy, dVal)
			y.Add(y, dy)
		} else {
			diff := new(big.Int).Sub(k, xy)
			dy := new(big.Int).Mul(diff, e18)
			dVal := d(x0, y, e18)
			if dVal.Sign() == 0 {
				return y
			}
			dy.Div(dy, dVal)
			y.Sub(y, dy)
		}

		// Convergence: |y - yPrev| <= 1
		delta := new(big.Int).Sub(y, yPrev)
		delta.Abs(delta)
		if delta.Cmp(big.NewInt(1)) <= 0 {
			return y
		}
	}

	return y
}
