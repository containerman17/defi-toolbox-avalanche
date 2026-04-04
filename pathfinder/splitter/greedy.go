package splitter

import (
	"time"

	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// Greedy runs N sequential BFS calls, each time with a PoolManagerOverlay
// reflecting dirty slots from previous chunks.
func Greedy(p *Params, amountIn *uint256.Int, chunks int) *Result {
	t0 := time.Now()

	chunkAmount := new(uint256.Int).Div(amountIn, uint256.NewInt(uint64(chunks)))
	if chunkAmount.IsZero() {
		return nil
	}

	accDirtySlots := make(map[common.Address]map[common.Hash]common.Hash)
	currentState := p.State
	evmCtx := statedb.GetCachedContext(p.EVMConfig)

	var result Result

	for i := 0; i < chunks; i++ {
		// Formula overlay: base PM for first chunk, overlay for subsequent
		var pqs formulas.PoolQuoterSource
		if i == 0 {
			pqs = p.PM
		} else {
			pqs = formulas.NewPoolManagerOverlay(p.BasePM, accDirtySlots)
		}

		// BFS
		route := pf.FindBestRoute(pqs, p.Adj, p.Pools, currentState, p.EVMConfig,
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

		// EVM-execute to capture dirty slots
		cs := statedb.NewCallState(currentState)
		ret, _, err := evmCtx.ExecuteWithCallState(cs, p.Sender, p.RouterAddr, route.Calldata)
		if err != nil || len(ret) < 32 {
			break
		}

		// Merge dirty slots
		for addr, slots := range cs.StorageOverrides() {
			if accDirtySlots[addr] == nil {
				accDirtySlots[addr] = make(map[common.Hash]common.Hash)
			}
			for slot, val := range slots {
				accDirtySlots[addr][slot] = val
			}
		}

		// Build state overlay for next chunk
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
