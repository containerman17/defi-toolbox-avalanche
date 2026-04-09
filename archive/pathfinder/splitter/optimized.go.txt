package splitter

import (
	"fmt"
	"time"

	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// Optimized discovers candidate paths via BFS once, then greedily allocates
// volume across those paths in chunks. Each chunk: formula-quote all paths
// (microseconds), pick the best, EVM-execute to capture pool depletion.
//
// This is much faster than Greedy (which runs full BFS per chunk across 2000+
// pools) while still accounting for pool depletion via EVM dirty slots.
func Optimized(p *Params, amountIn *uint256.Int, chunks int) *Result {
	t0 := time.Now()

	// ── Phase 1: Discover candidate paths ───────────────────────────
	// BFS at 5% volume to surface diverse paths including low-liquidity ones.
	// Also include the single best path at full volume as a guaranteed fallback.
	discoveryAmount := new(uint256.Int).Div(amountIn, uint256.NewInt(20))
	if discoveryAmount.IsZero() {
		discoveryAmount.Set(amountIn)
	}

	candidates := pf.FindTopRoutes(p.PM, p.Adj, p.Pools, p.State, p.EVMConfig,
		p.RouterAddr, p.Sender, p.TokenIn, p.TokenOut, discoveryAmount, p.MaxHops, 5)

	// Also discover at full volume — captures the path that's best for large sizes
	fullCandidates := pf.FindTopRoutes(p.PM, p.Adj, p.Pools, p.State, p.EVMConfig,
		p.RouterAddr, p.Sender, p.TokenIn, p.TokenOut, amountIn, p.MaxHops, 3)
	candidates = append(candidates, fullCandidates...)

	if len(candidates) == 0 {
		return nil
	}

	// Deduplicate by pool sequence
	type candidatePath struct {
		steps []pf.RouteStep
	}
	var paths []candidatePath
	seen := make(map[string]bool)
	for _, r := range candidates {
		k := pathKey(r.Steps)
		if seen[k] {
			continue
		}
		seen[k] = true
		paths = append(paths, candidatePath{steps: r.Steps})
	}

	// ── Phase 2: Greedy allocation across discovered paths ──────────
	// For each chunk, formula-quote all paths on the current (depleted)
	// state, pick the best, EVM-execute to track depletion.
	chunkAmount := new(uint256.Int).Div(amountIn, uint256.NewInt(uint64(chunks)))
	if chunkAmount.IsZero() {
		return nil
	}

	accDirtySlots := make(map[common.Address]map[common.Hash]common.Hash)
	currentState := p.State
	evmCtx := statedb.GetCachedContext(p.EVMConfig)
	dead := make([]bool, len(paths)) // paths that failed EVM

	var result Result
	var routed uint256.Int

	for chunk := 0; chunk < chunks; chunk++ {
		// Last chunk gets the remainder
		vol := new(uint256.Int).Set(chunkAmount)
		remaining := new(uint256.Int).Sub(amountIn, &routed)
		if chunk == chunks-1 || vol.Gt(remaining) {
			vol.Set(remaining)
		}
		if vol.IsZero() {
			break
		}

		// Formula-quote all live paths at chunk volume using overlay
		var pqs formulas.PoolQuoterSource
		if len(accDirtySlots) == 0 {
			pqs = p.PM
		} else {
			pqs = formulas.NewPoolManagerOverlay(p.BasePM, accDirtySlots)
		}

		bestPath := -1
		var bestOut uint256.Int
		for i := range paths {
			if dead[i] {
				continue
			}
			out := pf.QuotePath(pqs, paths[i].steps, vol)
			if out.IsZero() {
				continue
			}
			if bestPath < 0 || out.Gt(&bestOut) {
				bestOut = out
				bestPath = i
			}
		}
		if bestPath < 0 {
			break // no path can handle this chunk
		}

		// EVM-execute winning path
		calldata := buildCalldata(paths[bestPath].steps, vol)
		cs := statedb.NewCallState(currentState)
		ret, gasUsed, err := evmCtx.ExecuteWithCallState(cs, p.Sender, p.RouterAddr, calldata)
		if err != nil || len(ret) < 32 {
			dead[bestPath] = true
			chunk-- // retry this chunk with remaining paths
			continue
		}
		var evmOut uint256.Int
		evmOut.SetBytes(ret[:32])
		if evmOut.Bytes32()[0]&0x80 != 0 || evmOut.IsZero() {
			dead[bestPath] = true
			chunk-- // retry
			continue
		}

		result.Legs = append(result.Legs, Leg{
			Steps:   paths[bestPath].steps,
			Volume:  *vol,
			Output:  evmOut,
			GasUsed: gasUsed,
		})
		result.Total.Add(&result.Total, &evmOut)
		result.TotalGas += gasUsed
		routed.Add(&routed, vol)

		// Merge dirty slots
		for addr, slots := range cs.StorageOverrides() {
			if accDirtySlots[addr] == nil {
				accDirtySlots[addr] = make(map[common.Hash]common.Hash)
			}
			for slot, val := range slots {
				accDirtySlots[addr][slot] = val
			}
		}

		// Rebuild state overlay
		currentState = p.State.NewOverlay()
		for addr, slots := range accDirtySlots {
			for slot, val := range slots {
				currentState.SetStorageSlot(addr, slot, val)
			}
		}
	}

	result.ElapsedUs = time.Since(t0).Microseconds()
	return &result
}

func pathKey(steps []pf.RouteStep) string {
	key := ""
	for _, s := range steps {
		key += fmt.Sprintf("%x,", s.Pool)
	}
	return key
}
