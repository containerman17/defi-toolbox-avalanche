package splitter

import (
	"time"

	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// GreedyFast is Greedy with an incremental overlay that avoids recreating
// the PoolManagerOverlay from scratch each chunk.
//
// Standard Greedy creates a new PoolManagerOverlay per chunk, which rebuilds
// a fresh scratchPM (empty caches, pool structs rebuilt from storage). With
// 100 chunks, that's 100 cold-cache rebuilds.
//
// GreedyFast creates the overlay once and calls UpdateDirtySlots after each
// chunk. Only pools whose storage slots actually changed are invalidated —
// all other pools keep their cached quoters and quote results. This makes
// subsequent chunks much faster since typically only 2-3 pools change.
func GreedyFast(p *Params, amountIn *uint256.Int, chunks int) *Result {
	t0 := time.Now()

	chunkAmount := new(uint256.Int).Div(amountIn, uint256.NewInt(uint64(chunks)))
	if chunkAmount.IsZero() {
		return nil
	}

	accDirtySlots := make(map[common.Address]map[common.Hash]common.Hash)
	evmCtx := statedb.GetCachedContext(p.EVMConfig)

	// Create both overlays once; update incrementally.
	var formulaOverlay *formulas.PoolManagerOverlay
	stateOverlay := p.State.NewOverlay()

	var result Result

	for i := 0; i < chunks; i++ {
		var pqs formulas.PoolQuoterSource
		if i == 0 {
			pqs = p.PM
		} else if formulaOverlay == nil {
			// First chunk with dirty slots — create the overlay.
			formulaOverlay = formulas.NewPoolManagerOverlay(p.BasePM, accDirtySlots)
			pqs = formulaOverlay
		} else {
			pqs = formulaOverlay
		}

		// BFS
		route := pf.FindBestRoute(pqs, p.Adj, p.Pools, stateOverlay, p.EVMConfig,
			p.RouterAddr, p.Sender, p.TokenIn, p.TokenOut, chunkAmount, p.MaxHops)
		if route == nil {
			break
		}

		result.Legs = append(result.Legs, Leg{
			Steps:   route.Steps,
			Volume:  *chunkAmount,
			Output:  *route.AmountOut,
			GasUsed: route.GasUsed,
		})
		result.Total.Add(&result.Total, route.AmountOut)
		result.TotalGas += route.GasUsed

		// EVM-execute to capture dirty slots.
		cs := statedb.NewCallState(stateOverlay)
		ret, _, err := evmCtx.ExecuteWithCallState(cs, p.Sender, p.RouterAddr, route.Calldata)
		if err != nil || len(ret) < 32 {
			break
		}

		// Collect new dirty slots from this chunk.
		newDirty := cs.StorageOverrides()

		// Merge into accumulator and apply to state overlay incrementally.
		for addr, slots := range newDirty {
			if accDirtySlots[addr] == nil {
				accDirtySlots[addr] = make(map[common.Hash]common.Hash)
			}
			for slot, val := range slots {
				accDirtySlots[addr][slot] = val
				stateOverlay.SetStorageSlot(addr, slot, val)
			}
		}

		// Incrementally update formula overlay (only invalidate newly-dirty pools).
		if formulaOverlay != nil {
			formulaOverlay.UpdateDirtySlots(newDirty)
		}
	}

	result.ElapsedUs = time.Since(t0).Microseconds()
	return &result
}
