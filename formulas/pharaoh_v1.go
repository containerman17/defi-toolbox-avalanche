package formulas

import (
	"encoding/binary"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
	"github.com/holiman/uint256"
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
	Decimals0   *uint256.Int // 10^(token0 decimals), e.g. 1e6 for USDC
	Decimals1   *uint256.Int
	Reserve0    *uint256.Int
	Reserve1    *uint256.Int
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

// pharaohV1FactoryAddress is the PairFactory proxy used by all Pharaoh V1 pools.
const pharaohV1FactoryAddress = "0xAAA16c016BF556fcD620328f0759252E29b1AB57"

// Factory storage layout (PairFactory via Initializable proxy):
//   Slot 8:  stableFee (uint256)
//   Slot 9:  volatileFee (uint256)
//   Slot 15: _pairFee mapping (address => uint256)
const (
	pharaohV1FactoryStableFeeSlot   = 8
	pharaohV1FactoryVolatileFeeSlot = 9
	pharaohV1FactoryPairFeeBaseSlot = 15
)

// pharaohV1PairFeeStorageKey computes the storage key for _pairFee[pool] on
// the Pharaoh V1 factory: keccak256(abi.encode(pool, 15)).
func pharaohV1PairFeeStorageKey(pool common.Address) common.Hash {
	// abi.encode(address, uint256) = 32-byte left-padded address + 32-byte slot
	var data [64]byte
	copy(data[12:32], pool.Bytes())
	data[63] = pharaohV1FactoryPairFeeBaseSlot
	return common.BytesToHash(crypto.Keccak256(data[:]))
}

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

var e18 = uint256.NewInt(1_000_000_000_000_000_000)

// QuotePharaohV1 computes the output amount for a Pharaoh V1 swap.
// zeroForOne: true if swapping token0 -> token1.
func QuotePharaohV1(state *PharaohV1State, amountIn *uint256.Int, zeroForOne bool) *uint256.Int {
	out := quotePharaohV1Internal(state, amountIn, zeroForOne, state.FeeBps)
	if out == nil {
		return nil
	}
	if state.SubtractOne && !out.IsZero() {
		out.SubUint64(out, 1)
	}
	return out
}

func quotePharaohV1Internal(state *PharaohV1State, amountIn *uint256.Int, zeroForOne bool, feeBps int) *uint256.Int {
	// Apply fee: adjustedIn = amountIn - (amountIn * feeBps / 10000)
	var fee, adjustedIn uint256.Int
	feeBpsU := uint256.NewInt(uint64(feeBps))
	tenK := uint256.NewInt(10000)
	fee.Mul(amountIn, feeBpsU)
	fee.Div(&fee, tenK)
	adjustedIn.Sub(amountIn, &fee)

	return pharaohGetAmountOut(state, &adjustedIn, zeroForOne)
}

// pharaohGetAmountOut implements the core pricing logic (after fee deduction).
func pharaohGetAmountOut(state *PharaohV1State, amountIn *uint256.Int, zeroForOne bool) *uint256.Int {
	if state.Stable {
		return getAmountOutStable(state, amountIn, zeroForOne)
	}
	return getAmountOutVolatile(state, amountIn, zeroForOne)
}

// getAmountOutVolatile: amountOut = adjustedIn * reserveB / (reserveA + adjustedIn)
func getAmountOutVolatile(state *PharaohV1State, amountIn *uint256.Int, zeroForOne bool) *uint256.Int {
	var reserveA, reserveB *uint256.Int
	if zeroForOne {
		reserveA = state.Reserve0
		reserveB = state.Reserve1
	} else {
		reserveA = state.Reserve1
		reserveB = state.Reserve0
	}

	// amountOut = amountIn * reserveB / (reserveA + amountIn)
	var num, den, result uint256.Int
	num.Mul(amountIn, reserveB)
	den.Add(reserveA, amountIn)
	result.Div(&num, &den)
	return new(uint256.Int).Set(&result)
}

// getAmountOutStable implements the x³y + y³x = k stable curve.
// This mirrors the Solidity _getAmountOut for stable pairs.
func getAmountOutStable(state *PharaohV1State, amountIn *uint256.Int, zeroForOne bool) *uint256.Int {
	// Compute invariant k from raw reserves
	xy := kStable(state.Reserve0, state.Reserve1, state.Decimals0, state.Decimals1)

	// Normalize reserves to 18 decimals
	var r0Norm, r1Norm uint256.Int
	r0Norm.Mul(state.Reserve0, e18)
	r0Norm.Div(&r0Norm, state.Decimals0)
	r1Norm.Mul(state.Reserve1, e18)
	r1Norm.Div(&r1Norm, state.Decimals1)

	var reserveA, reserveB *uint256.Int
	var decimalsIn, decimalsOut *uint256.Int
	if zeroForOne {
		reserveA = &r0Norm
		reserveB = &r1Norm
		decimalsIn = state.Decimals0
		decimalsOut = state.Decimals1
	} else {
		reserveA = &r1Norm
		reserveB = &r0Norm
		decimalsIn = state.Decimals1
		decimalsOut = state.Decimals0
	}

	// Normalize amountIn to 18 decimals
	var amountInNorm uint256.Int
	amountInNorm.Mul(amountIn, e18)
	amountInNorm.Div(&amountInNorm, decimalsIn)

	// y = reserveB - get_y(amountInNorm + reserveA, xy, reserveB)
	var xNew uint256.Int
	xNew.Add(&amountInNorm, reserveA)
	yNew := getY(&xNew, &xy, reserveB)
	if yNew == nil || yNew.IsZero() {
		// nil: uint256 overflow in Solidity (getAmountOut() reverts)
		// zero: Newton-Raphson converged to zero — output would equal
		// the entire reserve, which Solidity's swap() rejects (amount < reserve).
		return nil
	}
	var dy uint256.Int
	dy.Sub(reserveB, yNew)

	// De-normalize to output token decimals
	dy.Mul(&dy, decimalsOut)
	dy.Div(&dy, e18)

	// The on-chain swap() requires amountOut < raw reserve (strict less-than).
	// If the computed output equals or exceeds the raw reserve, the swap will
	// revert with the K invariant check. Return nil to signal unswappable.
	var rawReserveOut *uint256.Int
	if zeroForOne {
		rawReserveOut = state.Reserve1
	} else {
		rawReserveOut = state.Reserve0
	}
	if !dy.Lt(rawReserveOut) {
		return nil
	}

	return new(uint256.Int).Set(&dy)
}

// kStable computes the invariant: k = (x * y / 1e18) * (x² / 1e18 + y² / 1e18) / 1e18
// where x, y are normalized to 18 decimals.
func kStable(reserve0, reserve1, decimals0, decimals1 *uint256.Int) uint256.Int {
	var x, y uint256.Int
	x.Mul(reserve0, e18)
	x.Div(&x, decimals0)
	y.Mul(reserve1, e18)
	y.Div(&y, decimals1)

	// _a = (x * y) / 1e18
	var a uint256.Int
	a.Mul(&x, &y)
	a.Div(&a, e18)

	// _b = (x*x/1e18 + y*y/1e18)
	var xx, yy, b uint256.Int
	xx.Mul(&x, &x)
	xx.Div(&xx, e18)
	yy.Mul(&y, &y)
	yy.Div(&yy, e18)
	b.Add(&xx, &yy)

	// k = (a * b) / 1e18
	var k uint256.Int
	k.Mul(&a, &b)
	k.Div(&k, e18)
	return k
}

// pharaohF computes: f(x0, y) = (x0 * y / 1e18) * (x0² / 1e18 + y² / 1e18) / 1e18
// Returns nil if a*b would overflow uint256 (mirrors Solidity arithmetic revert).
func pharaohF(x0, y *uint256.Int) *uint256.Int {
	var a uint256.Int
	a.Mul(x0, y)
	a.Div(&a, e18)

	var xx, yy, b uint256.Int
	xx.Mul(x0, x0)
	xx.Div(&xx, e18)
	yy.Mul(y, y)
	yy.Div(&yy, e18)
	b.Add(&xx, &yy)

	// Solidity uses uint256; if a*b overflows uint256, getAmountOut() reverts.
	var result uint256.Int
	_, overflow := result.MulOverflow(&a, &b)
	if overflow {
		return nil
	}
	result.Div(&result, e18)
	return new(uint256.Int).Set(&result)
}

// pharaohD computes the derivative: d(x0, y) = 3 * x0 * (y² / 1e18) / 1e18 + (x0² / 1e18 * x0) / 1e18
func pharaohD(x0, y *uint256.Int) uint256.Int {
	// 3 * x0 * (y*y/1e18) / 1e18
	var yy, term1 uint256.Int
	yy.Mul(y, y)
	yy.Div(&yy, e18)
	three := uint256.NewInt(3)
	term1.Mul(three, x0)
	term1.Mul(&term1, &yy)
	term1.Div(&term1, e18)

	// ((x0*x0/1e18) * x0) / 1e18
	var xx, term2 uint256.Int
	xx.Mul(x0, x0)
	xx.Div(&xx, e18)
	term2.Mul(&xx, x0)
	term2.Div(&term2, e18)

	var result uint256.Int
	result.Add(&term1, &term2)
	return result
}

// FetchPharaohV1StateStorage reads pool state using storage reads instead of eth_call.
// Uses the hardcoded registry for immutable data (fee, decimals, stable, subtractOne)
// and reads only the reserves via StorageReader. Returns nil, nil if pool is not in registry.
func FetchPharaohV1StateStorage(reader StorageReader, poolAddress string) (*PharaohV1State, error) {
	cfg, ok := pharaohV1Registry[poolAddress]
	if !ok {
		return nil, nil // not in registry, caller should fall back
	}

	state := &PharaohV1State{
		Decimals0:   uint256.NewInt(cfg.Decimals0),
		Decimals1:   uint256.NewInt(cfg.Decimals1),
		Stable:      cfg.Stable,
		FeeBps:      cfg.Fee,
		SubtractOne: cfg.SubtractOne,
	}

	addr := common.HexToAddress(poolAddress)

	if cfg.PackedSlot >= 0 {
		// Packed layout: ts(32)|r1(112)|r0(112) in one slot
		packedSlotHash := slotHash(int64(cfg.PackedSlot))
		data := reader(addr, packedSlotHash)
		// r0 = lower 112 bits (bytes 18..31), r1 = next 112 bits (bytes 4..17)
		var val, mask112 uint256.Int
		val.SetBytes(data[:])
		mask112.Sub(new(uint256.Int).Lsh(uint256.NewInt(1), 112), uint256.NewInt(1))
		state.Reserve0 = new(uint256.Int).And(&val, &mask112)
		state.Reserve1 = new(uint256.Int).And(new(uint256.Int).Rsh(&val, 112), &mask112)

		// Fee is mutable on-chain; read it from slot 16 (per-million) and convert to bps
		feeSlotHash := slotHash(16)
		feeData := reader(addr, feeSlotHash)
		var feePerMillion uint256.Int
		feePerMillion.SetBytes(feeData[:])
		if !feePerMillion.IsZero() {
			state.FeeBps = int(feePerMillion.Uint64() / 100)
		}
	} else {
		// Separate reserve slots
		r0SlotHash := slotHash(int64(cfg.Reserve0Slot))
		r0data := reader(addr, r0SlotHash)
		r1SlotHash := slotHash(int64(cfg.Reserve1Slot))
		r1data := reader(addr, r1SlotHash)
		state.Reserve0 = new(uint256.Int).SetBytes(r0data[:])
		state.Reserve1 = new(uint256.Int).SetBytes(r1data[:])

		// Beacon-proxy pools store the factory at the proxyImplSlot. Read it
		// to determine if this pool delegates fee reads to the PairFactory.
		// Non-beacon pools (older Solidly forks) use internal fee storage and
		// the registry value is authoritative.
		proxySlotHash := common.HexToHash(proxyImplSlot)
		factoryData := reader(addr, proxySlotHash)
		factoryAddr := common.BytesToAddress(factoryData[:])
		if factoryAddr == common.HexToAddress(pharaohV1FactoryAddress) {
			// Fee is mutable on-chain via factory.setPairFee(). Read it from the
			// factory's _pairFee mapping (base slot 15). If zero, fall back to the
			// factory's volatileFee (slot 9) or stableFee (slot 8).
			pairFeeKey := pharaohV1PairFeeStorageKey(addr)
			pairFeeData := reader(factoryAddr, pairFeeKey)
			var pairFeeVal uint256.Int
			pairFeeVal.SetBytes(pairFeeData[:])
			if !pairFeeVal.IsZero() {
				state.FeeBps = int(pairFeeVal.Uint64())
			} else {
				// No per-pool override; read factory default
				defaultSlot := pharaohV1FactoryVolatileFeeSlot
				if cfg.Stable {
					defaultSlot = pharaohV1FactoryStableFeeSlot
				}
				defaultFeeData := reader(factoryAddr, slotHash(int64(defaultSlot)))
				var defaultFee uint256.Int
				defaultFee.SetBytes(defaultFeeData[:])
				if !defaultFee.IsZero() {
					state.FeeBps = int(defaultFee.Uint64())
				}
			}
		}
	}

	return state, nil
}

// slotHash converts a small non-negative int to a common.Hash (big-endian 32 bytes).
func slotHash(n int64) common.Hash {
	var h common.Hash
	binary.BigEndian.PutUint64(h[24:], uint64(n))
	return h
}

// getY implements Newton-Raphson iteration to find y such that f(x0, y) = xy (the invariant).
// This mirrors the Solidity _get_y function exactly.
// Returns nil if the computation would overflow uint256 (Solidity revert behavior).
func getY(x0, xy, y0 *uint256.Int) *uint256.Int {
	var y uint256.Int
	y.Set(y0)

	for i := 0; i < 255; i++ {
		var yPrev uint256.Int
		yPrev.Set(&y)
		k := pharaohF(x0, &y)
		if k == nil {
			// uint256 overflow — mirrors Solidity revert
			return nil
		}

		if k.Lt(xy) {
			var diff, dy uint256.Int
			diff.Sub(xy, k)
			dy.Mul(&diff, e18)
			dVal := pharaohD(x0, &y)
			if dVal.IsZero() {
				return new(uint256.Int).Set(&y)
			}
			dy.Div(&dy, &dVal)
			y.Add(&y, &dy)
		} else {
			var diff, dy uint256.Int
			diff.Sub(k, xy)
			dy.Mul(&diff, e18)
			dVal := pharaohD(x0, &y)
			if dVal.IsZero() {
				return new(uint256.Int).Set(&y)
			}
			dy.Div(&dy, &dVal)
			// In Solidity 0.8+, y - dy reverts on underflow.
			// Clamp to zero to match: the pool can't be swapped at this size.
			if dy.Gt(&y) {
				y.Clear()
			} else {
				y.Sub(&y, &dy)
			}
		}

		// Convergence: |y - yPrev| <= 1
		var delta uint256.Int
		if y.Gt(&yPrev) {
			delta.Sub(&y, &yPrev)
		} else {
			delta.Sub(&yPrev, &y)
		}
		if !delta.Gt(uint256.NewInt(1)) {
			return new(uint256.Int).Set(&y)
		}
	}

	return new(uint256.Int).Set(&y)
}
