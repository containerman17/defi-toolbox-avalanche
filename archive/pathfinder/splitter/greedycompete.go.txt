package splitter

import (
	"time"

	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// GreedyCompete processes volume in slabs. For each slab, it competes:
//   - Single: one BFS + EVM at full slab volume
//   - Chunked: multiple smaller BFS + EVM at sub-slab volume
//
// Whichever produces more output wins. The winner's dirty slots are applied
// before the next slab. This naturally handles the "don't split when it
// doesn't help" case — at small volumes or when one path dominates, the
// single shot wins. At large volumes with diverse paths, chunks win.
func GreedyCompete(p *Params, amountIn *uint256.Int, slabPct, chunkPct uint64) *Result {
	t0 := time.Now()

	onePercent := new(uint256.Int).Div(amountIn, uint256.NewInt(100))
	if onePercent.IsZero() {
		return nil
	}
	slabVol := new(uint256.Int).Mul(onePercent, uint256.NewInt(slabPct))
	chunkVol := new(uint256.Int).Mul(onePercent, uint256.NewInt(chunkPct))

	accDirtySlots := make(map[common.Address]map[common.Hash]common.Hash)
	stateOverlay := p.State.NewOverlay()
	evmCtx := statedb.GetCachedContext(p.EVMConfig)
	var formulaOverlay *formulas.PoolManagerOverlay
	var result Result
	var routed uint256.Int

	maxSlabs := 100

	for slab := 0; slab < maxSlabs; slab++ {
		remaining := new(uint256.Int).Sub(amountIn, &routed)
		if remaining.IsZero() {
			break
		}

		thisSlabVol := new(uint256.Int).Set(slabVol)
		if thisSlabVol.Gt(remaining) {
			thisSlabVol.Set(remaining)
		}

		var pqs formulas.PoolQuoterSource
		if slab == 0 {
			pqs = p.PM
		} else if formulaOverlay == nil {
			formulaOverlay = formulas.NewPoolManagerOverlay(p.BasePM, accDirtySlots)
			pqs = formulaOverlay
		} else {
			pqs = formulaOverlay
		}

		// ── Contestant 1: single shot at full slab volume ───────────
		singleRoute := pf.FindBestRoute(pqs, p.Adj, p.Pools, stateOverlay, p.EVMConfig,
			p.RouterAddr, p.Sender, p.TokenIn, p.TokenOut, thisSlabVol, p.MaxHops)

		var singleOut uint256.Int
		var singleGas uint64
		var singleDirty map[common.Address]map[common.Hash]common.Hash

		if singleRoute != nil {
			calldata := buildCalldata(singleRoute.Steps, thisSlabVol)
			cs := statedb.NewCallState(stateOverlay)
			ret, gasUsed, err := evmCtx.ExecuteWithCallState(cs, p.Sender, p.RouterAddr, calldata)
			if err == nil && len(ret) >= 32 {
				singleOut.SetBytes(ret[:32])
				if singleOut.Bytes32()[0]&0x80 != 0 {
					singleOut.Clear()
				} else {
					singleGas = gasUsed
					singleDirty = cs.StorageOverrides()
				}
			}
		}

		// ── Contestant 2: chunked at sub-slab volume ────────────────
		var chunkedLegs []Leg
		var chunkedTotal uint256.Int
		var chunkedGas uint64
		chunkedDirty := make(map[common.Address]map[common.Hash]common.Hash)

		// Work on a temporary state overlay (don't pollute the main one)
		tempState := stateOverlay // shares the same base
		var tempFormula *formulas.PoolManagerOverlay

		chunkedRouted := new(uint256.Int)
		for chunk := 0; ; chunk++ {
			chunkRemaining := new(uint256.Int).Sub(thisSlabVol, chunkedRouted)
			if chunkRemaining.IsZero() {
				break
			}
			vol := new(uint256.Int).Set(chunkVol)
			if vol.Gt(chunkRemaining) {
				vol.Set(chunkRemaining)
			}
			if vol.IsZero() {
				break
			}

			var cpqs formulas.PoolQuoterSource
			if chunk == 0 {
				cpqs = pqs // same as slab-level quoter
			} else if tempFormula == nil {
				// Build overlay from slab-level dirty + chunk dirty
				merged := mergeDirtySlots(accDirtySlots, chunkedDirty)
				tempFormula = formulas.NewPoolManagerOverlay(p.BasePM, merged)
				cpqs = tempFormula
			} else {
				cpqs = tempFormula
			}

			route := pf.FindBestRoute(cpqs, p.Adj, p.Pools, tempState, p.EVMConfig,
				p.RouterAddr, p.Sender, p.TokenIn, p.TokenOut, vol, p.MaxHops)
			if route == nil {
				break
			}

			calldata := buildCalldata(route.Steps, vol)
			cs := statedb.NewCallState(tempState)
			ret, gasUsed, err := evmCtx.ExecuteWithCallState(cs, p.Sender, p.RouterAddr, calldata)
			if err != nil || len(ret) < 32 {
				break
			}
			var evmOut uint256.Int
			evmOut.SetBytes(ret[:32])
			if evmOut.Bytes32()[0]&0x80 != 0 || evmOut.IsZero() {
				break
			}

			chunkedLegs = append(chunkedLegs, Leg{
				Steps:   route.Steps,
				Volume:  *vol,
				Output:  evmOut,
				GasUsed: gasUsed,
			})
			chunkedTotal.Add(&chunkedTotal, &evmOut)
			chunkedGas += gasUsed
			chunkedRouted.Add(chunkedRouted, vol)

			// Accumulate chunk dirty slots
			for addr, slots := range cs.StorageOverrides() {
				if chunkedDirty[addr] == nil {
					chunkedDirty[addr] = make(map[common.Hash]common.Hash)
				}
				for slot, val := range slots {
					chunkedDirty[addr][slot] = val
				}
			}

			// Rebuild temp state with chunk dirty
			tempState = p.State.NewOverlay()
			merged := mergeDirtySlots(accDirtySlots, chunkedDirty)
			for addr, slots := range merged {
				for slot, val := range slots {
					tempState.SetStorageSlot(addr, slot, val)
				}
			}
			if tempFormula != nil {
				tempFormula.UpdateDirtySlots(cs.StorageOverrides())
			}
		}

		// ── Pick the winner ─────────────────────────────────────────
		useSingle := singleOut.Gt(&chunkedTotal) || (singleOut.Eq(&chunkedTotal) && singleGas <= chunkedGas)

		if useSingle && !singleOut.IsZero() {
			result.Legs = append(result.Legs, Leg{
				Steps:   singleRoute.Steps,
				Volume:  *thisSlabVol,
				Output:  singleOut,
				GasUsed: singleGas,
			})
			result.Total.Add(&result.Total, &singleOut)
			result.TotalGas += singleGas
			routed.Add(&routed, thisSlabVol)

			// Apply single's dirty slots
			for addr, slots := range singleDirty {
				if accDirtySlots[addr] == nil {
					accDirtySlots[addr] = make(map[common.Hash]common.Hash)
				}
				for slot, val := range slots {
					accDirtySlots[addr][slot] = val
					stateOverlay.SetStorageSlot(addr, slot, val)
				}
			}
		} else if !chunkedTotal.IsZero() {
			result.Legs = append(result.Legs, chunkedLegs...)
			result.Total.Add(&result.Total, &chunkedTotal)
			result.TotalGas += chunkedGas
			routed.Add(&routed, chunkedRouted)

			// Apply chunked dirty slots
			for addr, slots := range chunkedDirty {
				if accDirtySlots[addr] == nil {
					accDirtySlots[addr] = make(map[common.Hash]common.Hash)
				}
				for slot, val := range slots {
					accDirtySlots[addr][slot] = val
					stateOverlay.SetStorageSlot(addr, slot, val)
				}
			}
		} else {
			break
		}

		if formulaOverlay != nil {
			formulaOverlay.UpdateDirtySlots(accDirtySlots)
		} else {
			formulaOverlay = formulas.NewPoolManagerOverlay(p.BasePM, accDirtySlots)
		}
	}

	result.ElapsedUs = time.Since(t0).Microseconds()
	if len(result.Legs) == 0 {
		return nil
	}
	return &result
}

func mergeDirtySlots(a, b map[common.Address]map[common.Hash]common.Hash) map[common.Address]map[common.Hash]common.Hash {
	merged := make(map[common.Address]map[common.Hash]common.Hash)
	for addr, slots := range a {
		if merged[addr] == nil {
			merged[addr] = make(map[common.Hash]common.Hash)
		}
		for slot, val := range slots {
			merged[addr][slot] = val
		}
	}
	for addr, slots := range b {
		if merged[addr] == nil {
			merged[addr] = make(map[common.Hash]common.Hash)
		}
		for slot, val := range slots {
			merged[addr][slot] = val
		}
	}
	return merged
}
