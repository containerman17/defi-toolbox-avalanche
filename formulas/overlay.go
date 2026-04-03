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
	base          *PoolManager
	dirtySlots    map[common.Address]map[common.Hash]common.Hash
	affectedPools map[common.Address]bool
	scratchPM     *PoolManager // lazily created, only for affected pools
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

// Quote returns a formula quote. Unaffected pools delegate to the base.
// Affected pools are rebuilt from dirty state on the scratch PoolManager.
func (o *PoolManagerOverlay) Quote(pool common.Address, amountIn *uint256.Int, tokenIn, tokenOut common.Address) uint256.Int {
	if !o.affectedPools[pool] {
		return o.base.Quote(pool, amountIn, tokenIn, tokenOut)
	}
	o.ensureScratch()
	return o.scratchPM.Quote(pool, amountIn, tokenIn, tokenOut)
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
