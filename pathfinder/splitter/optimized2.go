package splitter

import (
	"time"

	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// optimizedTwoPhase is the shared implementation for OptimizedV2 and OptimizedV3.
// Phase 1 runs the given schedule for path discovery; Phase 2 reallocates across
// those paths with fine 2% chunks. Returns the better of the two.
func optimizedTwoPhase(p *Params, amountIn *uint256.Int, sched ChunkSchedule) *Result {
	t0 := time.Now()

	phase1 := GreedyMixed(p, amountIn, sched)
	if phase1 == nil {
		return nil
	}

	paths := CollectPaths(phase1.Legs)
	phase2 := AllocateAcrossPaths(p, amountIn, paths)

	elapsed := time.Since(t0).Microseconds()
	if phase2 != nil && phase2.Total.Gt(&phase1.Total) {
		phase2.ElapsedUs = elapsed
		return phase2
	}
	phase1.ElapsedUs = elapsed
	return phase1
}

// OptimizedV2 uses SchedShuffle2 for path discovery.
// Phase 1 (shuf2) + Phase 2 (fine 2% reallocation on discovered paths).
func OptimizedV2(p *Params, amountIn *uint256.Int) *Result {
	return optimizedTwoPhase(p, amountIn, SchedShuffle2)
}

// OptimizedV3 uses SchedFrontLoaded for path discovery.
// front discovers richer paths (30/36 near-best alone vs shuf2's 21/36),
// so Phase 2 reallocation should cover more of the remaining cases.
func OptimizedV3(p *Params, amountIn *uint256.Int) *Result {
	return optimizedTwoPhase(p, amountIn, SchedFrontLoaded)
}

// OptimizedV4 combines path discovery from both front and shuf2 schedules,
// then allocates across the union. front and shuf2 discover complementary
// paths under depletion — front finds paths via large initial chunks, shuf2
// finds paths via interleaved re-discovery mid-depletion.
func OptimizedV4(p *Params, amountIn *uint256.Int) *Result {
	t0 := time.Now()

	// Phase 1A: discover via front-loaded schedule
	phase1a := GreedyMixed(p, amountIn, SchedFrontLoaded)
	if phase1a == nil {
		return nil
	}

	// Phase 1B: discover via shuf2 schedule
	phase1b := GreedyMixed(p, amountIn, SchedShuffle2)

	// Combine path sets
	paths := CollectPaths(phase1a.Legs)
	if phase1b != nil {
		seen := make(map[string]bool)
		for _, p := range paths {
			seen[pathKey(p)] = true
		}
		for _, leg := range phase1b.Legs {
			k := pathKey(leg.Steps)
			if !seen[k] {
				seen[k] = true
				paths = append(paths, leg.Steps)
			}
		}
	}

	// Phase 2: allocate across combined paths
	phase2 := AllocateAcrossPaths(p, amountIn, paths)

	// Return best of all three
	best := phase1a
	if phase1b != nil && phase1b.Total.Gt(&best.Total) {
		best = phase1b
	}
	if phase2 != nil && phase2.Total.Gt(&best.Total) {
		best = phase2
	}

	best.ElapsedUs = time.Since(t0).Microseconds()
	return best
}

// CollectPaths extracts the unique route step sequences from a result's legs.
func CollectPaths(legs []Leg) [][]pf.RouteStep {
	seen := make(map[string]bool)
	var paths [][]pf.RouteStep
	for _, leg := range legs {
		k := pathKey(leg.Steps)
		if seen[k] {
			continue
		}
		seen[k] = true
		paths = append(paths, leg.Steps)
	}
	return paths
}

// AllocateAcrossPaths does greedy chunk allocation using only formula quotes for
// path selection (no BFS per chunk). EVM is still called per chunk for dirty-slot
// tracking. Uses uniform 2% chunks.
//
// This is Phase 2 of OptimizedV2 and can be called directly with a cached path set.
func AllocateAcrossPaths(p *Params, amountIn *uint256.Int, paths [][]pf.RouteStep) *Result {
	t0 := time.Now()

	onePercent := new(uint256.Int).Div(amountIn, uint256.NewInt(100))
	if onePercent.IsZero() {
		return nil
	}
	chunkVol := new(uint256.Int).Mul(onePercent, uint256.NewInt(2))
	if chunkVol.IsZero() {
		return nil
	}

	stateOverlay := p.State.NewOverlay()
	accDirtySlots := make(map[common.Address]map[common.Hash]common.Hash)
	evmCtx := statedb.GetCachedContext(p.EVMConfig)
	dead := make([]bool, len(paths))

	var formulaOverlay *formulas.PoolManagerOverlay
	var result Result
	var routed uint256.Int

	for {
		remaining := new(uint256.Int).Sub(amountIn, &routed)
		if remaining.IsZero() {
			break
		}
		vol := new(uint256.Int).Set(chunkVol)
		if vol.Gt(remaining) {
			vol.Set(remaining)
		}

		// Formula overlay: base PM initially, incremental overlay after first dirty slots
		var pqs formulas.PoolQuoterSource
		if len(accDirtySlots) == 0 {
			pqs = p.PM
		} else if formulaOverlay == nil {
			formulaOverlay = formulas.NewPoolManagerOverlay(p.BasePM, accDirtySlots)
			pqs = formulaOverlay
		} else {
			pqs = formulaOverlay
		}

		bestIdx := -1
		var bestOut uint256.Int
		for i, path := range paths {
			if dead[i] {
				continue
			}
			out := pf.QuotePath(pqs, path, vol)
			if out.IsZero() {
				continue
			}
			if bestIdx < 0 || out.Gt(&bestOut) {
				bestOut = out
				bestIdx = i
			}
		}
		if bestIdx < 0 {
			break
		}

		calldata := buildCalldata(paths[bestIdx], vol)
		cs := statedb.NewCallState(stateOverlay)
		ret, gasUsed, err := evmCtx.ExecuteWithCallState(cs, p.Sender, p.RouterAddr, calldata)
		if err != nil || len(ret) < 32 {
			dead[bestIdx] = true
			continue
		}
		var evmOut uint256.Int
		evmOut.SetBytes(ret[:32])
		if evmOut.Bytes32()[0]&0x80 != 0 || evmOut.IsZero() {
			dead[bestIdx] = true
			continue
		}

		result.Legs = append(result.Legs, Leg{
			Steps:   paths[bestIdx],
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

	if len(result.Legs) == 0 {
		return nil
	}
	result.ElapsedUs = time.Since(t0).Microseconds()
	return &result
}
