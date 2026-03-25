package formulas

import (
	"strings"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// PoolQuoter is implemented by each pool type's struct.
// Quote() is pure math — no state access, no keccak, no map lookups.
type PoolQuoter interface {
	// Quote returns the output amount for a given input. Pure math, no state access.
	Quote(amountIn *uint256.Int, zeroForOne bool) (*uint256.Int, bool)

	// Address returns the pool's contract address.
	Address() common.Address
}

// PoolManager holds pool structs and handles lazy construction + invalidation.
type PoolManager struct {
	pools       map[common.Address]PoolQuoter
	registry    *Registry
	reader      StorageReader
	poolTokens  map[common.Address][2]common.Address // pool → [token0, token1]
	poolTypes   map[common.Address]int               // pool → poolType from pools.txt
	poolDex     map[common.Address]string            // pool → DEX provider name (e.g. "pangolin_v2")
	tokenModels *TokenModelRegistry
}

// NewPoolManager creates a PoolManager backed by the given registry and storage reader.
func NewPoolManager(registry *Registry, reader StorageReader) *PoolManager {
	return &PoolManager{
		pools:       make(map[common.Address]PoolQuoter),
		registry:    registry,
		reader:      reader,
		poolTokens:  make(map[common.Address][2]common.Address),
		poolTypes:   make(map[common.Address]int),
		poolDex:     make(map[common.Address]string),
		tokenModels: NewTokenModelRegistry(),
	}
}

// SetPoolType registers the pool type and DEX provider for a pool (from pools.txt).
// This allows Get() to try V2 formula for pools marked -1 in registry.txt
// when the pool type indicates a V2-family DEX.
func (pm *PoolManager) SetPoolType(pool common.Address, poolType int, dex string) {
	pm.poolTypes[pool] = poolType
	pm.poolDex[pool] = dex
}

// isV2Family returns true if pool has poolType 8 (V2 family).
func (pm *PoolManager) isV2Family(pool common.Address) bool {
	pt, hasPT := pm.poolTypes[pool]
	return hasPT && pt == 8
}

// SetPoolTokens registers the token pair for a pool, enabling FoT adjustment.
func (pm *PoolManager) SetPoolTokens(pool common.Address, token0, token1 common.Address) {
	pm.poolTokens[pool] = [2]common.Address{token0, token1}
}

// Get returns a PoolQuoter for the given pool, building it if needed.
// Returns nil if the pool has no formula or construction fails.
// If the pool has FoT tokens, the returned quoter adjusts input/output automatically.
func (pm *PoolManager) Get(pool common.Address) (pq PoolQuoter) {
	if q, ok := pm.pools[pool]; ok {
		return q
	}

	formulaID, known := pm.registry.GetFormulaID(pool)
	poolHexLower := strings.ToLower(pool.Hex())
	if !known || formulaID < 0 {
		// Check lfjV2Registry as fallback — pools may be missing from registry.txt
		// or marked -1 due to function-based formula direction bug (now fixed in struct path)
		if _, inLFJ := lfjV2Registry[poolHexLower]; inLFJ {
			formulaID = FormulaLFJV2
		} else if !known {
			if _, inV3 := v3PoolFees[poolHexLower]; inV3 {
				formulaID = FormulaV3
			} else {
				return nil
			}
		} else if _, inV3 := v3PoolFees[poolHexLower]; inV3 {
			// V3 pools marked -1 in registry.txt can still use the V3 formula —
			// they were likely invalidated for reasons that don't apply to the
			// struct-based V3 quoter (e.g. dynamic fees now read from storage).
			formulaID = FormulaV3
		} else if pm.isV2Family(pool) {
			// V2-family pools marked -1: retry with V2 formula. The -1 was set by
			// formula discovery which compared function-based output to EVM. Common
			// causes: (a) FoT tokens not yet in fotCalculators (now handled by
			// fotPoolQuoter wrapper), (b) transient state during discovery,
			// (c) non-standard V2 forks (hurricane, fraxswap) with different
			// storage layout / fees — handled with DEX-specific constructors.
			formulaID = FormulaV2_30bps
		} else if _, inPharaoh := pharaohV1Registry[poolHexLower]; inPharaoh {
			// Pharaoh V1 pools marked -1: retry with Pharaoh V1 formula. The -1
			// was set by formula discovery which tested function-based quoting.
			// Common causes: FoT tokens (now handled by fotPoolQuoter wrapper),
			// or transient state during discovery.
			formulaID = FormulaPharaohV1
		} else {
			// known but formulaID < 0, and not in lfjV2Registry or v3PoolFees
			return nil
		}
	}

	// FoT: check if pool tokens require rebasing/formula-issue fallback.
	// Skip for V2 constant product — the formula is simple enough that it always
	// matches EVM output (both read the same slot 8 reserves). FoT taxes for V2
	// are handled by the fotPoolQuoter wrapper via fotCalculators.
	// Skip for V3 — the swap formula uses sqrtPrice/liquidity/ticks from storage,
	// not token balances. Rebasing and formula-issue tokens don't affect V3 math.
	// FoT taxes are handled by the fotPoolQuoter wrapper.
	// Fraxswap TWAMM pools are marked -1 in registry.txt so they never reach here.
	tokens, hasTokens := pm.poolTokens[pool]
	if hasTokens && formulaID != FormulaV2_30bps && formulaID != FormulaV3 {
		t0Hex := strings.ToLower(tokens[0].Hex())
		t1Hex := strings.ToLower(tokens[1].Hex())
		if FotRebasingTokens[t0Hex] || FotRebasingTokens[t1Hex] ||
			FotFormulaIssueTokens[t0Hex] || FotFormulaIssueTokens[t1Hex] {
			return nil
		}
	}

	// Recover from panics during construction
	defer func() {
		if r := recover(); r != nil {
			pq = nil
		}
	}()

	// Build inner pool struct (concrete type checks to avoid nil-interface issue)
	poolExempt := hasTokens && IsFotExemptPool(strings.ToLower(pool.Hex()))
	var model0, model1 TokenModel
	if hasTokens && !poolExempt {
		t0Hex := strings.ToLower(tokens[0].Hex())
		t1Hex := strings.ToLower(tokens[1].Hex())
		model0 = pm.tokenModels.GetModel(t0Hex)
		model1 = pm.tokenModels.GetModel(t1Hex)
	}
	wantFot := model0 != nil && model1 != nil && (model0.IsFoT() || model1.IsFoT())

	wrapAndCache := func(inner PoolQuoter) PoolQuoter {
		if wantFot {
			poolHex := strings.ToLower(pool.Hex())
			wrapped := &fotPoolQuoter{
				inner:         inner,
				model0:        model0,
				model1:        model1,
				inputExempt:   IsFotExemptInputPool(poolHex),
				outputExempt:  IsFotExemptOutputPool(poolHex),
			}
			pm.pools[pool] = wrapped
			return wrapped
		}
		pm.pools[pool] = inner
		return inner
	}

	switch formulaID {
	case FormulaV2_30bps:
		var p *V2Pool
		switch pm.poolDex[pool] {
		case "hurricane":
			p = newHurricanePool(pool, pm.reader)
		case "fraxswap":
			p = newFraxswapPool(pool, pm.reader)
		default:
			p = newV2Pool(pool, pm.reader)
		}
		if p != nil { return wrapAndCache(p) }
	case FormulaPharaohV1:
		if p := newPharaohV1Pool(pool, pm.reader); p != nil { return wrapAndCache(p) }
	case FormulaV3:
		if p := newV3Pool(pool, pm.reader); p != nil { return wrapAndCache(p) }
	case FormulaDODO:
		var token0 common.Address
		if hasTokens {
			token0 = tokens[0]
		}
		if p := newDODOPool(pool, pm.reader, token0); p != nil { return wrapAndCache(p) }
	case FormulaLFJV2:
		if hasTokens {
			if p := newLFJV2Pool(pool, pm.reader, tokens[0], tokens[1]); p != nil { return wrapAndCache(p) }
		}
	case FormulaV4:
		if p := newV4Pool(pool, pm.reader); p != nil { return wrapAndCache(p) }
	case FormulaBalancerV3:
		if p := newBalancerV3Pool(pool, pm.reader); p != nil { return wrapAndCache(p) }
	}
	// Pool has a formula type but construction failed (empty/uninitialized).
	// Cache a dead quoter so we don't retry construction or fall through to EVM.
	dead := &deadPoolQuoter{addr: pool}
	pm.pools[pool] = dead
	return dead
}

// deadPoolQuoter is a no-op quoter cached for pools where construction failed.
// Prevents EVM fallback and re-construction attempts for empty/uninitialized pools.
type deadPoolQuoter struct {
	addr common.Address
}

func (d *deadPoolQuoter) Quote(amountIn *uint256.Int, zeroForOne bool) (*uint256.Int, bool) {
	return nil, false
}

func (d *deadPoolQuoter) Address() common.Address {
	return d.addr
}

// Invalidate drops the cached pool struct for a given address.
func (pm *PoolManager) Invalidate(addr common.Address) {
	delete(pm.pools, addr)
}

// InvalidateAll drops all cached pool structs.
func (pm *PoolManager) InvalidateAll() {
	pm.pools = make(map[common.Address]PoolQuoter)
}

// poolHex returns the lowercase hex string for a pool address.
func poolHex(addr common.Address) string {
	return strings.ToLower(addr.Hex())
}

// fotPoolQuoter wraps a PoolQuoter to apply FoT tax adjustments on input/output.
type fotPoolQuoter struct {
	inner          PoolQuoter
	model0         TokenModel // token0's model
	model1         TokenModel // token1's model
	inputExempt    bool       // true if pool is in FotExemptInputPools (fee skipped when pool is recipient)
	outputExempt   bool       // true if pool is in FotExemptOutputPools (fee skipped when pool is sender)
}

func (f *fotPoolQuoter) Address() common.Address {
	return f.inner.Address()
}

func (f *fotPoolQuoter) Quote(amountIn *uint256.Int, zeroForOne bool) (*uint256.Int, bool) {
	var modelIn, modelOut TokenModel
	if zeroForOne {
		modelIn = f.model0
		modelOut = f.model1
	} else {
		modelIn = f.model1
		modelOut = f.model0
	}

	// Adjust input: if tokenIn is FoT, pool receives less.
	// Skip if this pool is exempt on the input side (token's transfer skips fee when to==pool).
	var effectiveIn *uint256.Int
	if f.inputExempt {
		effectiveIn = amountIn
	} else {
		effectiveIn = modelIn.AdjustInput(amountIn)
		if effectiveIn == nil {
			return nil, false
		}
	}

	out, ok := f.inner.Quote(effectiveIn, zeroForOne)
	if !ok {
		return nil, false
	}

	// Adjust output: if tokenOut is FoT, user receives less.
	// Skip if this pool is exempt on the output side (token skips fee when pool is sender).
	if !f.outputExempt {
		out = modelOut.AdjustOutput(out)
		if out == nil {
			return nil, false
		}
	}

	return out, true
}

// Exported wrappers for experiments
func CachedKeccakSlotBytes(key int64, mappingSlot [32]byte) [32]byte {
	return cachedKeccakSlotBytes(key, mappingSlot)
}
func V3FloorDivExported(a, b int) int { return v3FloorDiv(a, b) }
