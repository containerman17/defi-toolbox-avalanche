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
	pools    map[common.Address]PoolQuoter
	registry *Registry
	reader   StorageReader
}

// NewPoolManager creates a PoolManager backed by the given registry and storage reader.
func NewPoolManager(registry *Registry, reader StorageReader) *PoolManager {
	return &PoolManager{
		pools:    make(map[common.Address]PoolQuoter),
		registry: registry,
		reader:   reader,
	}
}

// Get returns a PoolQuoter for the given pool, building it if needed.
// Returns nil if the pool has no formula or construction fails.
func (pm *PoolManager) Get(pool common.Address) (pq PoolQuoter) {
	if q, ok := pm.pools[pool]; ok {
		return q
	}

	formulaID, known := pm.registry.GetFormulaID(pool)
	if !known || formulaID < 0 {
		return nil
	}

	// Recover from panics during construction
	defer func() {
		if r := recover(); r != nil {
			pq = nil
		}
	}()

	switch formulaID {
	case FormulaV2_30bps:
		if p := newV2Pool(pool, pm.reader); p != nil {
			pm.pools[pool] = p
			return p
		}
	case FormulaPharaohV1:
		if p := newPharaohV1Pool(pool, pm.reader); p != nil {
			pm.pools[pool] = p
			return p
		}
	case FormulaV3:
		if p := newV3Pool(pool, pm.reader); p != nil {
			pm.pools[pool] = p
			return p
		}
	case FormulaDODO:
		if p := newDODOPool(pool, pm.reader); p != nil {
			pm.pools[pool] = p
			return p
		}
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

// Exported wrappers for experiments
func CachedKeccakSlotBytes(key int64, mappingSlot [32]byte) [32]byte {
	return cachedKeccakSlotBytes(key, mappingSlot)
}
func V3FloorDivExported(a, b int) int { return v3FloorDiv(a, b) }
