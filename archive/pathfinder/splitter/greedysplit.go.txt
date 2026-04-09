package splitter

import (
	"time"

	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// GreedySplit decouples discovery volume from execution volume.
//
// BFS discovers different paths at different volumes (topK=3 beam keeps
// different candidates). A pool that's top-3 at 10% volume might not be
// top-3 at 2%. So we discover at a large probe volume to find the best
// paths, but execute at a small volume for low price impact.
//
// Each chunk:
//  1. BFS at probePercent% of remaining volume → finds the best path
//  2. Execute at execPercent% of total volume → low price impact
//
// This gives large-volume discovery quality with small-volume precision.
func GreedySplit(p *Params, amountIn *uint256.Int, probePct, execPct uint64) *Result {
	t0 := time.Now()

	execVol := new(uint256.Int).Mul(
		new(uint256.Int).Div(amountIn, uint256.NewInt(100)),
		uint256.NewInt(execPct),
	)
	if execVol.IsZero() {
		return nil
	}

	accDirtySlots := make(map[common.Address]map[common.Hash]common.Hash)
	stateOverlay := p.State.NewOverlay()
	evmCtx := statedb.GetCachedContext(p.EVMConfig)
	var formulaOverlay *formulas.PoolManagerOverlay
	var result Result
	var routed uint256.Int

	maxLegs := 200 // safety cap

	for i := 0; i < maxLegs; i++ {
		remaining := new(uint256.Int).Sub(amountIn, &routed)
		if remaining.IsZero() {
			break
		}

		// Execution volume: execPct% of total, capped at remaining.
		vol := new(uint256.Int).Set(execVol)
		if vol.Gt(remaining) {
			vol.Set(remaining)
		}

		// Probe volume: probePct% of remaining, for BFS discovery.
		probeVol := new(uint256.Int).Mul(
			new(uint256.Int).Div(remaining, uint256.NewInt(100)),
			uint256.NewInt(probePct),
		)
		if probeVol.IsZero() {
			probeVol.Set(remaining)
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

		// BFS at probe volume to discover the best path.
		route := pf.FindBestRoute(pqs, p.Adj, p.Pools, stateOverlay, p.EVMConfig,
			p.RouterAddr, p.Sender, p.TokenIn, p.TokenOut, probeVol, p.MaxHops)
		if route == nil {
			break
		}

		// Execute at the smaller execution volume using the discovered path.
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
