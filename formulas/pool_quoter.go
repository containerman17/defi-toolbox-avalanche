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

// EVMCaller executes a view call against the current EVM state.
// Returns (result, true) on success, (nil, false) on revert or error.
type EVMCaller func(to common.Address, data []byte) ([]byte, bool)

// PoolManager holds pool structs and handles lazy construction + invalidation.
type PoolManager struct {
	pools          map[common.Address]PoolQuoter
	registry       *Registry
	reader         StorageReader
	evmCaller      EVMCaller // optional; used for rate provider calls (Balancer V3 WITH_RATE tokens)
	poolTokens     map[common.Address][2]common.Address // pool → [token0, token1]
	poolTypes      map[common.Address]int               // pool → poolType from pools.txt
	poolDex        map[common.Address]string            // pool → DEX provider name (e.g. "pangolin_v2")
	tokenModels    *TokenModelRegistry
	blockTimestamp uint64 // block.timestamp for volatility reference updates (LFJ V2)
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
		tokenModels: NewTokenModelRegistry(reader),
	}
}

// SetEVMCaller provides an EVM execution function used for calling rate providers
// in Balancer V3 WITH_RATE token pools. Must be called before Get() for those pools.
func (pm *PoolManager) SetEVMCaller(fn EVMCaller) {
	pm.evmCaller = fn
}

// SetBlockTimestamp sets the block timestamp used for LFJ V2 volatility reference
// updates. Must be called before Get() to ensure correct fee calculation.
func (pm *PoolManager) SetBlockTimestamp(ts uint64) {
	pm.blockTimestamp = ts
}

// SetPoolType registers the pool type and DEX provider for a pool (from pools.txt).
// DEX name is used to dispatch V2 variants (hurricane, fraxswap) to correct constructors.
func (pm *PoolManager) SetPoolType(pool common.Address, poolType int, dex string) {
	pm.poolTypes[pool] = poolType
	pm.poolDex[pool] = dex
}

// SetPoolTokens registers the token pair for a pool, enabling FoT adjustment.
func (pm *PoolManager) SetPoolTokens(pool common.Address, token0, token1 common.Address) {
	pm.poolTokens[pool] = [2]common.Address{token0, token1}
}

// Get returns a PoolQuoter for the given pool, building it if needed.
// Returns nil if the pool has no formula in the registry (→ EVM fallback).
// The registry is authoritative: -1 means EVM, positive means formula.
// If the pool has FoT tokens, the returned quoter adjusts input/output automatically.
func (pm *PoolManager) Get(pool common.Address) (pq PoolQuoter) {
	if q, ok := pm.pools[pool]; ok {
		return q
	}

	formulaID, known := pm.registry.GetFormulaID(pool)
	if !known || formulaID < 0 {
		return nil // not in registry or marked invalid → EVM fallback
	}

	return pm.buildQuoter(pool, formulaID)
}

// BuildQuoterForFormulaID builds a PoolQuoter for a pool using the given formula ID,
// bypassing the registry. Used by discover to test candidate formulas before they
// are registered. The quoter is NOT cached — each call builds a fresh quoter.
func (pm *PoolManager) BuildQuoterForFormulaID(pool common.Address, formulaID int) PoolQuoter {
	pm.Invalidate(pool) // clear any cached quoter
	return pm.buildQuoter(pool, formulaID)
}

func (pm *PoolManager) buildQuoter(pool common.Address, formulaID int) (pq PoolQuoter) {
	tokens, hasTokens := pm.poolTokens[pool]

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
			// newLFJV2Pool always returns a non-nil PoolQuoter (nullLFJV2Pool for
			// pools that cannot be formula-quoted), preventing EVM fallback for all
			// LFJ V2 pools regardless of whether they are in lfjV2Registry or not.
			return wrapAndCache(newLFJV2Pool(pool, pm.reader, tokens[0], tokens[1], pm.blockTimestamp))
		}
	case FormulaV4:
		if p := newV4Pool(pool, pm.reader); p != nil { return wrapAndCache(p) }
	case FormulaBalancerV3:
		if p := newBalancerV3Pool(pool, pm.reader, pm.evmCaller); p != nil { return wrapAndCache(p) }
	case FormulaBalancerV2:
		if p := newBalancerV2Pool(pool, pm.reader); p != nil { return wrapAndCache(p) }
	}
	// Construction failed — return nil for EVM fallback.
	// The registry verified this formula works via multi-amount testing,
	// so failure here is transient (empty pool, missing state).
	return nil
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
