package splitter

import (
	"time"

	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// ChunkSchedule defines a mixed chunk size pattern.
// Percentages of original amount. If they don't sum to 100, the last
// entry is repeated until the total is routed.
type ChunkSchedule struct {
	Name   string
	Chunks []uint64 // each entry is a percentage (1 = 1% of total)
}

// Pre-defined schedules for benchmarking.
var (
	// FrontLoaded: big first, steep drop to 1% tail. ~36 chunks.
	SchedFrontLoaded = ChunkSchedule{"front", []uint64{
		15, 12, 10, 8, 5, 5, 3, 3, 3, 2, 2, 2, 2, 2, 2, 2, 2,
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, // 100%
	}}

	// Gradual: smooth taper from 6% to 2%. ~32 chunks.
	SchedGradual = ChunkSchedule{"grad", []uint64{
		6, 6, 5, 5, 5, 5, 4, 4, 4, 4, 3, 3, 3, 3, 3,
		2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, // 100%
	}}

	// Plateau: step-down blocks. ~30 chunks.
	SchedPlateau = ChunkSchedule{"plat", []uint64{
		8, 8, 8, 8, 8, // 40%
		4, 4, 4, 4, 4, // 60%
		2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, // 100%
	}}

	// Magic: W=17 L=0 in early testing. 2×10% + 4×5% + 2% tail. ~36 chunks.
	SchedMagic = ChunkSchedule{"magic", []uint64{
		10, 10, 5, 5, 5, 5, // 40%, rest in 2% chunks
		2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2,
		2, 2, 2, 2, 2, 2, 2, 2, 2, 2, // 100%
	}}

	// Shuffled variants: interleave large and small chunks.
	// The theory: re-discovering at large volume mid-way catches paths
	// that only become visible after partial depletion.

	// Shuffle1: 10, 2, 2, 10, 2, 2, 5, 2, 2, 5, 2, 2... alternating discovery and fine-tune.
	SchedShuffle1 = ChunkSchedule{"shuf1", []uint64{
		10, 2, 2, 10, 2, 2, 5, 2, 2, 5, 2, 2, 5, 2, 2, 5, 2, 2, // 62%
		2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, // 100%
	}}

	// Shuffle2: 10, 5, 2, 10, 5, 2, 10, 5, 2... repeating pattern.
	SchedShuffle2 = ChunkSchedule{"shuf2", []uint64{
		10, 5, 2, 10, 5, 2, 10, 5, 2, // 51%
		5, 2, 5, 2, 5, 2, 5, 2, 5, 2, 5, 2, 2, 2, 2, // 100% (49% tail)
	}}

	// Shuffle3: big-small-big-small zigzag.
	SchedShuffle3 = ChunkSchedule{"shuf3", []uint64{
		10, 2, 8, 2, 6, 2, 5, 2, 4, 2, 3, 2, 3, 2, 3, 2, 3, 2, // 62%
		2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, // 100%
	}}

	// Bulk: 40% upfront in one big chunk, then 2% tail.
	SchedBulk = ChunkSchedule{"bulk", []uint64{
		40, // 40%
		2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2,
		2, 2, 2, 2, 2, 2, 2, 2, 2, 2, // 100%
	}}

	// Shuffle4: pulse — large re-discovery every 5 chunks.
	SchedShuffle4 = ChunkSchedule{"shuf4", []uint64{
		10, 2, 2, 2, 2, // 18%
		8, 2, 2, 2, 2, // 34%
		6, 2, 2, 2, 2, // 48%
		4, 2, 2, 2, 2, // 60%
		2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, // 100%
	}}
)

// GreedyMixed uses a schedule of decreasing chunk sizes.
// If the schedule doesn't cover 100%, the last entry is repeated.
func GreedyMixed(p *Params, amountIn *uint256.Int, sched ChunkSchedule) *Result {
	t0 := time.Now()

	onePercent := new(uint256.Int).Div(amountIn, uint256.NewInt(100))
	if onePercent.IsZero() {
		return nil
	}

	accDirtySlots := make(map[common.Address]map[common.Hash]common.Hash)
	stateOverlay := p.State.NewOverlay()
	evmCtx := statedb.GetCachedContext(p.EVMConfig)
	var formulaOverlay *formulas.PoolManagerOverlay
	var result Result
	var routed uint256.Int

	lastPct := sched.Chunks[len(sched.Chunks)-1]

	for i := 0; ; i++ {
		remaining := new(uint256.Int).Sub(amountIn, &routed)
		if remaining.IsZero() {
			break
		}

		// Pick percentage from schedule, or repeat last entry.
		pct := lastPct
		if i < len(sched.Chunks) {
			pct = sched.Chunks[i]
		}

		vol := new(uint256.Int).Mul(onePercent, uint256.NewInt(pct))
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
