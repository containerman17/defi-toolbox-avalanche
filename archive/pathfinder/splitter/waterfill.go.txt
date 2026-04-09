package splitter

import (
	"time"

	pf "defi-toolbox/pathfinder"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// WaterFill finds the optimal volume split across discovered paths by binary-
// searching for the equilibrium marginal rate — the rate at which all active
// paths produce the same marginal output per unit of additional input.
//
// Unlike greedy (which greedily picks the best path per chunk) this solves the
// continuous optimum: allocate volume so that d(output)/d(input) is equal across
// all paths. The algorithm uses formula quotes only (~6000 calls, <10ms), then
// EVM-verifies the result once.
func WaterFill(p *Params, amountIn *uint256.Int) *Result {
	t0 := time.Now()

	// ── Phase 1: Discover candidate paths ───────────────────────────
	discoveryAmount := new(uint256.Int).Div(amountIn, uint256.NewInt(20))
	if discoveryAmount.IsZero() {
		discoveryAmount.Set(amountIn)
	}

	candidates := pf.FindTopRoutes(p.PM, p.Adj, p.Pools, p.State, p.EVMConfig,
		p.RouterAddr, p.Sender, p.TokenIn, p.TokenOut, discoveryAmount, p.MaxHops, 5)

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

	// ── Phase 2: Water-filling via binary search on marginal rate ────
	// delta = step size for numerical marginal (0.01% of volume, min 1)
	delta := new(uint256.Int).Div(amountIn, uint256.NewInt(10000))
	if delta.IsZero() {
		delta = uint256.NewInt(1)
	}

	pm := p.PM

	// marginal returns QuotePath(v+delta) - QuotePath(v) for a path.
	// This is the marginal output for delta more input.
	marginal := func(steps []pf.RouteStep, v *uint256.Int) uint256.Int {
		vPlusDelta := new(uint256.Int).Add(v, delta)
		outHigh := pf.QuotePath(pm, steps, vPlusDelta)
		if outHigh.IsZero() {
			return uint256.Int{}
		}
		outLow := pf.QuotePath(pm, steps, v)
		if outHigh.Lt(&outLow) {
			return uint256.Int{}
		}
		var m uint256.Int
		m.Sub(&outHigh, &outLow)
		return m
	}

	// Compute initial marginals at v=0 and drop dead paths
	type livePath struct {
		steps []pf.RouteStep
		m0    uint256.Int
	}
	var live []livePath
	var maxM0 uint256.Int
	for i := range paths {
		m := marginal(paths[i].steps, uint256.NewInt(0))
		if m.IsZero() {
			continue
		}
		live = append(live, livePath{steps: paths[i].steps, m0: m})
		if m.Gt(&maxM0) {
			maxM0 = m
		}
	}

	if len(live) == 0 {
		return nil
	}

	// findVolumeForMarginal: binary search on [0, maxVol] for volume where
	// marginal(v) ≈ targetMarginal. Marginal is monotonically decreasing.
	// Returns 0 if marginal(0) < target (path never competitive at this rate).
	findVolumeForMarginal := func(steps []pf.RouteStep, target *uint256.Int, maxVol *uint256.Int) uint256.Int {
		m0 := marginal(steps, uint256.NewInt(0))
		if m0.Lt(target) {
			return uint256.Int{} // path never reaches this marginal
		}

		lo := uint256.NewInt(0)
		hi := new(uint256.Int).Set(maxVol)

		for iter := 0; iter < 20; iter++ {
			mid := new(uint256.Int).Add(lo, hi)
			mid.Rsh(mid, 1)
			if mid.Eq(lo) {
				break
			}
			m := marginal(steps, mid)
			if m.Gt(target) {
				lo = mid // marginal still too high → need more volume
			} else {
				hi = mid // marginal too low → need less volume
			}
		}
		return *lo
	}

	// Binary search for equilibrium rate r* in [0, max(m0)]
	// At each rMid: sum volumes where marginal(v_i) = rMid.
	// If sum > amountIn → rate too low, raise. If sum < amountIn → rate too high, lower.
	rLow := uint256.NewInt(0)
	rHigh := new(uint256.Int).Set(&maxM0)

	// Allocations per path at the found equilibrium
	allocs := make([]uint256.Int, len(live))

	for iter := 0; iter < 30; iter++ {
		rMid := new(uint256.Int).Add(rLow, rHigh)
		rMid.Rsh(rMid, 1)
		if rMid.Eq(rLow) {
			break
		}

		var totalVol uint256.Int
		for i := range live {
			v := findVolumeForMarginal(live[i].steps, rMid, amountIn)
			allocs[i] = v
			totalVol.Add(&totalVol, &v)
		}

		if totalVol.Gt(amountIn) {
			rLow = rMid // rate too low → paths want too much volume → raise rate
		} else {
			rHigh = rMid // rate too high → not enough volume allocated → lower rate
		}
	}

	// Final allocation at rHigh (the converged rate)
	var totalAlloc uint256.Int
	for i := range live {
		v := findVolumeForMarginal(live[i].steps, rHigh, amountIn)
		allocs[i] = v
		totalAlloc.Add(&totalAlloc, &v)
	}

	// Fix rounding residual: add remainder to the path with highest marginal at current allocation
	var remainder uint256.Int
	if amountIn.Gt(&totalAlloc) {
		remainder.Sub(amountIn, &totalAlloc)
	}
	if !remainder.IsZero() {
		bestIdx := 0
		bestM := marginal(live[0].steps, &allocs[0])
		for i := 1; i < len(live); i++ {
			m := marginal(live[i].steps, &allocs[i])
			if m.Gt(&bestM) {
				bestM = m
				bestIdx = i
			}
		}
		allocs[bestIdx].Add(&allocs[bestIdx], &remainder)
	}

	// Zero out dust legs (< 0.1% of total volume)
	dustThreshold := new(uint256.Int).Div(amountIn, uint256.NewInt(1000))
	for i := range allocs {
		if allocs[i].Lt(dustThreshold) {
			allocs[i].Clear()
		}
	}

	// Redistribute cleared dust to the largest leg
	var checkSum uint256.Int
	largestIdx := 0
	for i := range allocs {
		checkSum.Add(&checkSum, &allocs[i])
		if allocs[i].Gt(&allocs[largestIdx]) {
			largestIdx = i
		}
	}
	if amountIn.Gt(&checkSum) {
		var dust uint256.Int
		dust.Sub(amountIn, &checkSum)
		allocs[largestIdx].Add(&allocs[largestIdx], &dust)
	}

	// ── Phase 3: EVM verification ───────────────────────────────────
	accDirtySlots := make(map[common.Address]map[common.Hash]common.Hash)
	currentState := p.State
	evmCtx := statedb.GetCachedContext(p.EVMConfig)

	var result Result

	for i := range live {
		if allocs[i].IsZero() {
			continue
		}

		vol := new(uint256.Int).Set(&allocs[i])
		calldata := buildCalldata(live[i].steps, vol)

		cs := statedb.NewCallState(currentState)
		ret, gasUsed, err := evmCtx.ExecuteWithCallState(cs, p.Sender, p.RouterAddr, calldata)
		if err != nil || len(ret) < 32 {
			continue // skip failed legs
		}
		var evmOut uint256.Int
		evmOut.SetBytes(ret[:32])
		if evmOut.Bytes32()[0]&0x80 != 0 || evmOut.IsZero() {
			continue
		}

		result.Legs = append(result.Legs, Leg{
			Steps:   live[i].steps,
			Volume:  *vol,
			Output:  evmOut,
			GasUsed: gasUsed,
		})
		result.Total.Add(&result.Total, &evmOut)
		result.TotalGas += gasUsed

		// Merge dirty slots for subsequent legs
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

	if len(result.Legs) == 0 {
		return nil
	}

	result.ElapsedUs = time.Since(t0).Microseconds()
	return &result
}
