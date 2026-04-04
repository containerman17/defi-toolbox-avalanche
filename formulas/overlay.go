package formulas

import (
	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// PoolManagerOverlay wraps a base PoolManager with dirty storage slots.
// Pools whose dependency slots are dirty get rebuilt lazily from an overlay
// reader; all other pools delegate to the base. Disposable — create one per
// split routing iteration.
type PoolManagerOverlay struct {
	base            *PoolManager
	dirtySlots      map[common.Address]map[common.Hash]common.Hash
	affectedPools   map[common.Address]bool
	scratchPM       *PoolManager                    // lazily created, only for affected pools
	forceQuoteCache bool                            // if true, cache all quotes including time-dependent pools
	quoteCacheMap   map[overlayQuoteKey]uint256.Int // overlay-level cache when forceQuoteCache is true
}

// NewPoolManagerOverlay creates an overlay that intercepts quotes for pools
// whose storage slots were dirtied. dirtySlots is (contractAddr → slot → value).
func NewPoolManagerOverlay(base *PoolManager, dirtySlots map[common.Address]map[common.Hash]common.Hash) *PoolManagerOverlay {
	affected := make(map[common.Address]bool)
	depSlots := base.DepSlots()
	for contractAddr, slots := range dirtySlots {
		if depMap, ok := depSlots[contractAddr]; ok {
			for slot := range slots {
				if poolAddrs, ok := depMap[slot]; ok {
					for _, pa := range poolAddrs {
						affected[pa] = true
					}
				}
			}
		}
	}
	return &PoolManagerOverlay{
		base:          base,
		dirtySlots:    dirtySlots,
		affectedPools: affected,
	}
}

// AffectedCount returns how many pools are affected by the dirty slots.
func (o *PoolManagerOverlay) AffectedCount() int {
	return len(o.affectedPools)
}

// UpdateDirtySlots merges new dirty slots into the overlay without recreating
// the scratch PoolManager. Only pools affected by the NEW slots are invalidated
// on the scratch PM — previously-built pools whose slots didn't change keep
// their cached quoters and quote caches. This is much faster than creating a
// new overlay when only 2-3 pools change per iteration.
func (o *PoolManagerOverlay) UpdateDirtySlots(newDirtySlots map[common.Address]map[common.Hash]common.Hash) {
	// Find which pools are newly affected by the new dirty slots.
	depSlots := o.base.DepSlots()
	var newlyAffected []common.Address

	for contractAddr, slots := range newDirtySlots {
		for slot, val := range slots {
			// Update stored dirty slots.
			if o.dirtySlots[contractAddr] == nil {
				o.dirtySlots[contractAddr] = make(map[common.Hash]common.Hash)
			}
			o.dirtySlots[contractAddr][slot] = val

			// Find pools affected by this slot.
			if depMap, ok := depSlots[contractAddr]; ok {
				if poolAddrs, ok := depMap[slot]; ok {
					for _, pa := range poolAddrs {
						if !o.affectedPools[pa] {
							o.affectedPools[pa] = true
							newlyAffected = append(newlyAffected, pa)
						} else {
							// Already affected — but the slot value changed,
							// so invalidate the cached quoter on scratchPM.
							newlyAffected = append(newlyAffected, pa)
						}
					}
				}
			}
		}
	}

	// Invalidate overlay-level quote cache entries for affected pools.
	if o.forceQuoteCache && len(newlyAffected) > 0 {
		affectedSet := make(map[common.Address]bool, len(newlyAffected))
		for _, pa := range newlyAffected {
			affectedSet[pa] = true
		}
		for key := range o.quoteCacheMap {
			if affectedSet[key.pool] {
				delete(o.quoteCacheMap, key)
			}
		}
	}

	// Update the overlay reader on scratchPM if it exists.
	if o.scratchPM != nil && len(newlyAffected) > 0 {
		// Update the overlay reader to use the latest dirty slots.
		baseReader := o.base.Reader()
		o.scratchPM.reader = func(addr common.Address, slot common.Hash) common.Hash {
			if slots, ok := o.dirtySlots[addr]; ok {
				if val, ok := slots[slot]; ok {
					return val
				}
			}
			return baseReader(addr, slot)
		}

		// Invalidate only newly-affected pools so they rebuild from updated state.
		for _, pa := range newlyAffected {
			o.scratchPM.Invalidate(pa)
			// Re-register pool metadata for rebuild.
			if tokens := o.base.GetPoolTokens(pa); len(tokens) >= 2 {
				o.scratchPM.SetPoolTokens(pa, tokens...)
			}
			poolType, dex := o.base.GetPoolTypeInfo(pa)
			o.scratchPM.SetPoolType(pa, poolType, dex)
		}
	}
}

// Quote returns a formula quote. Unaffected pools delegate to the base.
// Affected pools are rebuilt from dirty state on the scratch PoolManager.
// When forceQuoteCache is enabled, all quotes (including time-dependent pools
// like LFJ V2 that the base PM doesn't cache) are cached on the overlay.
func (o *PoolManagerOverlay) Quote(pool common.Address, amountIn *uint256.Int, tokenIn, tokenOut common.Address) uint256.Int {
	if o.forceQuoteCache {
		// Check overlay-level cache first.
		if o.quoteCacheMap == nil {
			o.quoteCacheMap = make(map[overlayQuoteKey]uint256.Int)
		}
		key := overlayQuoteKey{pool: pool, amountIn: *amountIn, tokenIn: tokenIn, tokenOut: tokenOut}
		if cached, ok := o.quoteCacheMap[key]; ok {
			return cached
		}
		var result uint256.Int
		if !o.affectedPools[pool] {
			result = o.base.QuoteBypassQuoteCache(pool, amountIn, tokenIn, tokenOut)
		} else {
			o.ensureScratch()
			result = o.scratchPM.QuoteBypassQuoteCache(pool, amountIn, tokenIn, tokenOut)
		}
		o.quoteCacheMap[key] = result
		return result
	}

	if !o.affectedPools[pool] {
		return o.base.Quote(pool, amountIn, tokenIn, tokenOut)
	}
	o.ensureScratch()
	return o.scratchPM.Quote(pool, amountIn, tokenIn, tokenOut)
}

type overlayQuoteKey struct {
	pool     common.Address
	amountIn uint256.Int
	tokenIn  common.Address
	tokenOut common.Address
}

// EnableQuoteCache forces the scratch PM to cache all quotes, including
// time-dependent pools (LFJ V2). Safe when the overlay is used within a
// single block — the block timestamp doesn't change between chunks.
func (o *PoolManagerOverlay) EnableQuoteCache() {
	o.forceQuoteCache = true
}

// ensureScratch creates the scratch PoolManager on first use.
func (o *PoolManagerOverlay) ensureScratch() {
	if o.scratchPM != nil {
		return
	}

	// Overlay reader: dirty slots first, then fall through to base reader.
	baseReader := o.base.Reader()
	overlayReader := func(addr common.Address, slot common.Hash) common.Hash {
		if slots, ok := o.dirtySlots[addr]; ok {
			if val, ok := slots[slot]; ok {
				return val
			}
		}
		return baseReader(addr, slot)
	}

	o.scratchPM = NewPoolManager(o.base.GetRegistry(), overlayReader)
	o.scratchPM.SetBlockTimestamp(o.base.GetBlockTimestamp())
	if caller := o.base.EVMCallerFn(); caller != nil {
		o.scratchPM.SetEVMCaller(caller)
	}

	// Copy metadata only for affected pools
	for pool := range o.affectedPools {
		if tokens := o.base.GetPoolTokens(pool); len(tokens) >= 2 {
			o.scratchPM.SetPoolTokens(pool, tokens...)
		}
		poolType, dex := o.base.GetPoolTypeInfo(pool)
		o.scratchPM.SetPoolType(pool, poolType, dex)
	}
}
