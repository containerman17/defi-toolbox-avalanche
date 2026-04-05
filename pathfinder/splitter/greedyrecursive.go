package splitter

import (
	"time"

	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// GreedyRecursive recursively splits volume, competing single vs split at each level.
//
// Start with 100% as a single shot. Then try splitting into two halves — for
// each half, recursively compete single vs split. If the two halves combined
// beat the single, use them. Recurse until chunks are below minPct%.
//
// This finds the optimal split depth automatically: deep liquidity pools get
// single large chunks, thin pools get many small chunks. Fast when splitting
// doesn't help (stops at level 1), only spends time when it finds real gains.
func GreedyRecursive(p *Params, amountIn *uint256.Int, minPct uint64) *Result {
	t0 := time.Now()

	onePercent := new(uint256.Int).Div(amountIn, uint256.NewInt(100))
	if onePercent.IsZero() {
		return nil
	}
	minVol := new(uint256.Int).Mul(onePercent, uint256.NewInt(minPct))
	if minVol.IsZero() {
		minVol.SetUint64(1)
	}

	evmCtx := statedb.GetCachedContext(p.EVMConfig)

	legs := recurseSplit(
		p, amountIn, minVol, evmCtx,
		p.State.NewOverlay(),
		make(map[common.Address]map[common.Hash]common.Hash),
		p.PM,
		0,
	)

	if len(legs) == 0 {
		return nil
	}

	var result Result
	for _, leg := range legs {
		result.Legs = append(result.Legs, leg)
		result.Total.Add(&result.Total, &leg.Output)
		result.TotalGas += leg.GasUsed
	}
	result.ElapsedUs = time.Since(t0).Microseconds()
	return &result
}

// recurseSplit tries a single shot at `vol`, then (if vol > minVol) splits
// into two halves and recurses. Returns whichever set of legs produces more.
func recurseSplit(
	p *Params,
	vol *uint256.Int,
	minVol *uint256.Int,
	evmCtx *statedb.CachedContext,
	stateOvl *statedb.StateDB,
	dirtySlots map[common.Address]map[common.Hash]common.Hash,
	pqs formulas.PoolQuoterSource,
	depth int,
) []Leg {
	if vol.IsZero() {
		return nil
	}

	// ── Single shot ─────────────────────────────────────────────────
	route := pf.FindBestRoute(pqs, p.Adj, p.Pools, stateOvl, p.EVMConfig,
		p.RouterAddr, p.Sender, p.TokenIn, p.TokenOut, vol, p.MaxHops)
	if route == nil {
		return nil
	}

	calldata := buildCalldata(route.Steps, vol)
	cs := statedb.NewCallState(stateOvl)
	ret, gasUsed, err := evmCtx.ExecuteWithCallState(cs, p.Sender, p.RouterAddr, calldata)
	if err != nil || len(ret) < 32 {
		return nil
	}
	var singleOut uint256.Int
	singleOut.SetBytes(ret[:32])
	if singleOut.Bytes32()[0]&0x80 != 0 || singleOut.IsZero() {
		return nil
	}

	singleLeg := Leg{
		Steps:   route.Steps,
		Volume:  *vol,
		Output:  singleOut,
		GasUsed: gasUsed,
	}

	// ── Base case: volume too small to split further ────────────────
	halfVol := new(uint256.Int).Rsh(vol, 1)
	if halfVol.Lt(minVol) || depth >= 8 {
		return []Leg{singleLeg}
	}

	// ── Recurse: try two halves ─────────────────────────────────────
	secondHalf := new(uint256.Int).Sub(vol, halfVol)

	// First half
	firstLegs := recurseSplit(p, halfVol, minVol, evmCtx, stateOvl, dirtySlots, pqs, depth+1)
	if len(firstLegs) == 0 {
		return []Leg{singleLeg}
	}

	// Apply first half's dirty slots for second half
	firstDirty := make(map[common.Address]map[common.Hash]common.Hash)
	// Re-execute first half's legs to get dirty slots
	tempState := stateOvl
	for _, leg := range firstLegs {
		cd := buildCalldata(leg.Steps, &leg.Volume)
		tcs := statedb.NewCallState(tempState)
		evmCtx.ExecuteWithCallState(tcs, p.Sender, p.RouterAddr, cd)
		for addr, slots := range tcs.StorageOverrides() {
			if firstDirty[addr] == nil {
				firstDirty[addr] = make(map[common.Hash]common.Hash)
			}
			for slot, val := range slots {
				firstDirty[addr][slot] = val
			}
		}
		tempState = p.State.NewOverlay()
		merged := mergeDirtySlots(dirtySlots, firstDirty)
		for addr, slots := range merged {
			for slot, val := range slots {
				tempState.SetStorageSlot(addr, slot, val)
			}
		}
	}

	// Build quoter for second half on depleted state
	mergedDirty := mergeDirtySlots(dirtySlots, firstDirty)
	secondPqs := formulas.NewPoolManagerOverlay(p.BasePM, mergedDirty)

	// Second half
	secondLegs := recurseSplit(p, secondHalf, minVol, evmCtx, tempState, mergedDirty, secondPqs, depth+1)
	if len(secondLegs) == 0 {
		return []Leg{singleLeg}
	}

	// ── Compare: single vs halves ───────────────────────────────────
	var halvesTotal uint256.Int
	for _, leg := range firstLegs {
		halvesTotal.Add(&halvesTotal, &leg.Output)
	}
	for _, leg := range secondLegs {
		halvesTotal.Add(&halvesTotal, &leg.Output)
	}

	if halvesTotal.Gt(&singleOut) {
		allLegs := make([]Leg, 0, len(firstLegs)+len(secondLegs))
		allLegs = append(allLegs, firstLegs...)
		allLegs = append(allLegs, secondLegs...)
		return allLegs
	}

	return []Leg{singleLeg}
}
