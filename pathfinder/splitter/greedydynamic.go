package splitter

import (
	"time"

	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// GreedyDynamic adapts chunk sizes based on whether the best path changed.
//
// Starts with a large discovery chunk. If the next BFS finds the SAME path,
// switches to small fine-tuning chunks (pool is stable, just keep going).
// If BFS finds a DIFFERENT path, does another large chunk (pool landscape
// shifted, worth probing at scale).
//
// This naturally adapts: early chunks (pools shifting fast) get large
// discovery; later chunks (stable paths) get fine-tuning.
//
// Optional: forceEvery > 0 means force a large chunk every N small chunks,
// even if the path didn't change. This catches cases where a different path
// would be better at large volume but the small-volume BFS can't see it.
func GreedyDynamic(p *Params, amountIn *uint256.Int, largePct, smallPct uint64) *Result {
	return greedyDynamicInner(p, amountIn, largePct, smallPct, 0)
}

// GreedyDynamicForced is GreedyDynamic with periodic forced re-discovery.
func GreedyDynamicForced(p *Params, amountIn *uint256.Int, largePct, smallPct uint64, forceEvery int) *Result {
	return greedyDynamicInner(p, amountIn, largePct, smallPct, forceEvery)
}

func greedyDynamicInner(p *Params, amountIn *uint256.Int, largePct, smallPct uint64, forceEvery int) *Result {
	t0 := time.Now()

	onePercent := new(uint256.Int).Div(amountIn, uint256.NewInt(100))
	if onePercent.IsZero() {
		return nil
	}
	largeVol := new(uint256.Int).Mul(onePercent, uint256.NewInt(largePct))
	smallVol := new(uint256.Int).Mul(onePercent, uint256.NewInt(smallPct))

	accDirtySlots := make(map[common.Address]map[common.Hash]common.Hash)
	stateOverlay := p.State.NewOverlay()
	evmCtx := statedb.GetCachedContext(p.EVMConfig)
	var formulaOverlay *formulas.PoolManagerOverlay
	var result Result
	var routed uint256.Int
	var prevPath string
	smallsSinceDiscovery := 0

	maxLegs := 200

	for i := 0; i < maxLegs; i++ {
		remaining := new(uint256.Int).Sub(amountIn, &routed)
		if remaining.IsZero() {
			break
		}

		// Decide chunk size: large if first chunk, path changed, or forced.
		vol := new(uint256.Int).Set(smallVol)
		forcedDiscovery := forceEvery > 0 && smallsSinceDiscovery >= forceEvery
		if i == 0 || prevPath == "" || forcedDiscovery {
			vol.Set(largeVol)
		}
		if vol.Gt(remaining) {
			vol.Set(remaining)
		}
		if vol.IsZero() {
			break
		}

		var pqs formulas.PoolQuoterSource
		if i == 0 {
			pqs = p.PM
		} else if formulaOverlay == nil {
			formulaOverlay = formulas.NewPoolManagerOverlay(p.BasePM, accDirtySlots)
			pqs = formulaOverlay
		} else {
			pqs = formulaOverlay
		}

		route := pf.FindBestRoute(pqs, p.Adj, p.Pools, stateOverlay, p.EVMConfig,
			p.RouterAddr, p.Sender, p.TokenIn, p.TokenOut, vol, p.MaxHops)
		if route == nil {
			break
		}

		currentPath := pathKey(route.Steps)

		// If path changed and we were using small chunks, redo this
		// iteration with a large chunk to discover at scale.
		if prevPath != "" && currentPath != prevPath && vol.Eq(smallVol) {
			vol.Set(largeVol)
			if vol.Gt(remaining) {
				vol.Set(remaining)
			}
			// Re-BFS at large volume — may find yet another path.
			route = pf.FindBestRoute(pqs, p.Adj, p.Pools, stateOverlay, p.EVMConfig,
				p.RouterAddr, p.Sender, p.TokenIn, p.TokenOut, vol, p.MaxHops)
			if route == nil {
				break
			}
			currentPath = pathKey(route.Steps)
		}

		if vol.Eq(smallVol) {
			smallsSinceDiscovery++
		} else {
			smallsSinceDiscovery = 0
		}
		prevPath = currentPath

		calldata := buildCalldata(route.Steps, vol)
		cs := statedb.NewCallState(stateOverlay)
		ret, gasUsed, err := evmCtx.ExecuteWithCallState(cs, p.Sender, p.RouterAddr, calldata)
		if err != nil || len(ret) < 32 {
			break
		}
		var evmOut uint256.Int
		evmOut.SetBytes(ret[:32])
		if evmOut.Bytes32()[0]&0x80 != 0 || evmOut.IsZero() {
			break
		}

		result.Legs = append(result.Legs, Leg{
			Steps:   route.Steps,
			Volume:  *vol,
			Output:  evmOut,
			GasUsed: gasUsed,
		})
		result.Total.Add(&result.Total, &evmOut)
		result.TotalGas += gasUsed
		routed.Add(&routed, vol)

		newDirty := cs.StorageOverrides()
		for addr, slots := range newDirty {
			if accDirtySlots[addr] == nil {
				accDirtySlots[addr] = make(map[common.Hash]common.Hash)
			}
			for slot, val := range slots {
				accDirtySlots[addr][slot] = val
				stateOverlay.SetStorageSlot(addr, slot, val)
			}
		}
		if formulaOverlay != nil {
			formulaOverlay.UpdateDirtySlots(newDirty)
		}
	}

	result.ElapsedUs = time.Since(t0).Microseconds()
	if len(result.Legs) == 0 {
		return nil
	}
	return &result
}
