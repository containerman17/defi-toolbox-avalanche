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
	pools      map[common.Address]PoolQuoter
	registry   *Registry
	reader     StorageReader
	poolTokens map[common.Address][2]common.Address // pool → [token0, token1]
}

// NewPoolManager creates a PoolManager backed by the given registry and storage reader.
func NewPoolManager(registry *Registry, reader StorageReader) *PoolManager {
	return &PoolManager{
		pools:      make(map[common.Address]PoolQuoter),
		registry:   registry,
		reader:     reader,
		poolTokens: make(map[common.Address][2]common.Address),
	}
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
	if !known || formulaID < 0 {
		return nil
	}

	// FoT: check if pool tokens require rebasing/formula-issue fallback
	tokens, hasTokens := pm.poolTokens[pool]
	if hasTokens {
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
	wantFot := hasTokens && !IsFotExemptPool(strings.ToLower(pool.Hex()))
	var t0Hex, t1Hex string
	if wantFot {
		t0Hex = strings.ToLower(tokens[0].Hex())
		t1Hex = strings.ToLower(tokens[1].Hex())
		wantFot = IsFotToken(t0Hex) || IsFotToken(t1Hex)
	}

	wrapAndCache := func(inner PoolQuoter) PoolQuoter {
		if wantFot {
			wrapped := &fotPoolQuoter{inner: inner, token0Hex: t0Hex, token1Hex: t1Hex}
			pm.pools[pool] = wrapped
			return wrapped
		}
		pm.pools[pool] = inner
		return inner
	}

	switch formulaID {
	case FormulaV2_30bps:
		if p := newV2Pool(pool, pm.reader); p != nil { return wrapAndCache(p) }
	case FormulaPharaohV1:
		if p := newPharaohV1Pool(pool, pm.reader); p != nil { return wrapAndCache(p) }
	case FormulaV3:
		if p := newV3Pool(pool, pm.reader); p != nil { return wrapAndCache(p) }
	case FormulaDODO:
		if p := newDODOPool(pool, pm.reader); p != nil { return wrapAndCache(p) }
	case FormulaV4:
		if p := newV4Pool(pool, pm.reader); p != nil { return wrapAndCache(p) }
	}
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
	inner     PoolQuoter
	token0Hex string // lowercase hex of token0
	token1Hex string // lowercase hex of token1
}

func (f *fotPoolQuoter) Address() common.Address {
	return f.inner.Address()
}

func (f *fotPoolQuoter) Quote(amountIn *uint256.Int, zeroForOne bool) (*uint256.Int, bool) {
	var tokenInHex, tokenOutHex string
	if zeroForOne {
		tokenInHex = f.token0Hex
		tokenOutHex = f.token1Hex
	} else {
		tokenInHex = f.token1Hex
		tokenOutHex = f.token0Hex
	}

	// Adjust input: if tokenIn is FoT, pool receives less
	effectiveIn := amountIn
	if fee, ok := FotCalcFee(tokenInHex, amountIn.ToBig()); ok {
		feeU256, overflow := uint256.FromBig(fee)
		if overflow {
			return nil, false
		}
		adjusted := new(uint256.Int).Sub(amountIn, feeU256)
		if adjusted.IsZero() || adjusted.Sign() < 0 {
			return nil, false
		}
		effectiveIn = adjusted
	}

	out, ok := f.inner.Quote(effectiveIn, zeroForOne)
	if !ok {
		return nil, false
	}

	// Adjust output: if tokenOut is FoT, user receives less
	if fee, hasFot := FotCalcFee(tokenOutHex, out.ToBig()); hasFot {
		feeU256, overflow := uint256.FromBig(fee)
		if overflow {
			return nil, false
		}
		out = new(uint256.Int).Sub(out, feeU256)
		if out.IsZero() || out.Sign() < 0 {
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
