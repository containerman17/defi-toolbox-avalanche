package pathfinder

import (
	"defi-toolbox/formulas"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// RouteStep represents one hop in a route.
type RouteStep struct {
	Pool     common.Address `json:"pool"`
	PoolType int            `json:"poolType"`
	TokenIn  common.Address `json:"tokenIn"`
	TokenOut common.Address `json:"tokenOut"`
}

// Route is the result of a pathfinding search.
type Route struct {
	Steps     []RouteStep  `json:"steps"`
	AmountOut *uint256.Int `json:"amountOut"`
	GasUsed   uint64       `json:"gasUsed"`
	Calldata  []byte       `json:"-"`
	Stats     RouteStats   `json:"stats"`
}

// RouteStats tracks quoting statistics.
type RouteStats struct {
	FormulaQuotes int     `json:"formulaQuotes"`
	EVMQuotes     int     `json:"evmQuotes"`
	TotalQuotes   int     `json:"totalQuotes"`
}

// PoolEdge is a directed edge in the token adjacency graph.
type PoolEdge struct {
	PoolIdx  uint16
	TokenOut common.Address
	TokenIn  common.Address
}

// BuildAdjacency creates a token→[]PoolEdge adjacency map from pools.
// Only includes pools known to the registry. Supports N-token pools (N*(N-1) edges).
func BuildAdjacency(pools []Pool, registry *formulas.Registry) map[common.Address][]PoolEdge {
	adj := make(map[common.Address][]PoolEdge)
	for i := range pools {
		p := &pools[i]
		if len(p.Tokens) < 2 {
			continue
		}
		if _, known := registry.GetFormulaID(p.Address); !known {
			continue
		}
		idx := uint16(i)
		for ti := range p.Tokens {
			for tj := range p.Tokens {
				if ti != tj {
					adj[p.Tokens[ti]] = append(adj[p.Tokens[ti]], PoolEdge{idx, p.Tokens[tj], p.Tokens[ti]})
				}
			}
		}
	}
	return adj
}

// DUMMY_SENDER is the from address for EVM calls.
var DUMMY_SENDER = common.HexToAddress("0x000000000000000000000000000000000000dEaD")

// ── BFS node ─────────────────────────────────────────────────────────

type bfsNode struct {
	amount   uint256.Int
	parentID int32  // index into allNodes (-1 for root)
	poolIdx  uint16 // index into pools slice
	tokenIn  common.Address
	token    common.Address // tokenOut / current token
}

const topK = 3

// ── FindBestRoute ────────────────────────────────────────────────────

// FindBestRoute finds the best swap route from tokenIn to tokenOut using
// formula BFS (same algorithm as arb3) with EVM verification.
//
// BFS expands layer by layer (up to maxHops), keeping top-3 amounts per
// token per layer with real cascading amounts. Paths reaching tokenOut are
// candidates. Top 5 are EVM-verified via full swap(); best is returned.
//
// stateWithOverrides should be a pre-built overlay with token balance and
// approval overrides already applied. The caller creates this once and reuses
// it across calls so that code hash caches persist.
func FindBestRoute(
	pm *formulas.PoolManager,
	adj map[common.Address][]PoolEdge,
	pools []Pool,
	stateWithOverrides *statedb.StateDB,
	cfg statedb.EVMConfig,
	routerAddr common.Address,
	sender common.Address,
	tokenIn, tokenOut common.Address,
	amountIn *uint256.Int,
	maxHops int,
) *Route {
	if amountIn.IsZero() {
		return nil
	}
	cyclic := tokenIn == tokenOut
	if maxHops <= 0 || maxHops > 4 {
		maxHops = 4
	}

	// ── Formula BFS ──────────────────────────────────────────────────

	// All nodes stored flat for backtracking
	allNodes := make([]bfsNode, 1, 1024)
	allNodes[0] = bfsNode{
		amount:   *amountIn,
		parentID: -1,
		token:    tokenIn,
	}

	// Current layer: indices into allNodes
	currentLayer := []int32{0}
	formulaQuotes := 0

	// Candidates: node indices that reached tokenOut
	type candidate struct {
		nodeIdx int32
	}
	var candidates []candidate

	for hop := 0; hop < maxHops; hop++ {
		// Per-token top-K for this layer
		type topEntry struct {
			amount  uint256.Int
			nodeIdx int32
		}
		tokenBest := make(map[common.Address][]topEntry)

		for _, parentIdx := range currentLayer {
			parent := &allNodes[parentIdx]

			for _, edge := range adj[parent.token] {
				// Don't use the same pool we arrived through
				if parent.parentID >= 0 && edge.PoolIdx == parent.poolIdx {
					continue
				}
				// Don't route back to tokenIn in A→B mode (avoid trivial loops)
				if !cyclic && edge.TokenOut == tokenIn {
					continue
				}

				formulaQuotes++
				out := pm.Quote(pools[edge.PoolIdx].Address, &parent.amount, edge.TokenIn, edge.TokenOut)
				if out.IsZero() {
					continue
				}

				tok := edge.TokenOut

				// Cyclic: edge back to start at hop >= 2 is a candidate, not a frontier entry
				if cyclic && tok == tokenOut && hop >= 1 {
					nodeIdx := int32(len(allNodes))
					allNodes = append(allNodes, bfsNode{
						amount:   out,
						parentID: parentIdx,
						poolIdx:  edge.PoolIdx,
						tokenIn:  edge.TokenIn,
						token:    tok,
					})
					candidates = append(candidates, candidate{nodeIdx: nodeIdx})
					continue
				}

				entries := tokenBest[tok]

				if len(entries) < topK {
					nodeIdx := int32(len(allNodes))
					allNodes = append(allNodes, bfsNode{
						amount:   out,
						parentID: parentIdx,
						poolIdx:  edge.PoolIdx,
						tokenIn:  edge.TokenIn,
						token:    tok,
					})
					tokenBest[tok] = append(entries, topEntry{amount: out, nodeIdx: nodeIdx})
				} else {
					// Find worst
					worstIdx := 0
					for j := 1; j < len(entries); j++ {
						if entries[j].amount.Lt(&entries[worstIdx].amount) {
							worstIdx = j
						}
					}
					if out.Gt(&entries[worstIdx].amount) {
						nodeIdx := int32(len(allNodes))
						allNodes = append(allNodes, bfsNode{
							amount:   out,
							parentID: parentIdx,
							poolIdx:  edge.PoolIdx,
							tokenIn:  edge.TokenIn,
							token:    tok,
						})
						entries[worstIdx] = topEntry{amount: out, nodeIdx: nodeIdx}
						tokenBest[tok] = entries
					}
				}
			}
		}

		// A→B mode: collect candidates reaching tokenOut
		if !cyclic {
			if entries, ok := tokenBest[tokenOut]; ok {
				for _, e := range entries {
					candidates = append(candidates, candidate{nodeIdx: e.nodeIdx})
				}
			}
		}

		// Build next frontier (exclude tokenOut — no point expanding past destination)
		currentLayer = currentLayer[:0]
		for tok, entries := range tokenBest {
			if tok == tokenOut {
				continue
			}
			for _, e := range entries {
				currentLayer = append(currentLayer, e.nodeIdx)
			}
		}

		if len(currentLayer) == 0 {
			break
		}
	}

	if len(candidates) == 0 {
		return nil
	}

	// Sort candidates by output descending (insertion sort, small slice)
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0; j-- {
			a := &allNodes[candidates[j].nodeIdx].amount
			b := &allNodes[candidates[j-1].nodeIdx].amount
			if a.Gt(b) {
				candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
			} else {
				break
			}
		}
	}

	// Backtrack a candidate to build steps
	backtrack := func(nodeIdx int32) ([]RouteStep, []common.Address, []int, []string) {
		// Collect hops in reverse
		var revSteps []RouteStep
		var poolAddrs []common.Address
		var poolTypes []int
		var extraDatas []string
		var tokenPairs []common.Address

		idx := nodeIdx
		for idx >= 0 && allNodes[idx].parentID >= 0 {
			node := &allNodes[idx]
			parent := &allNodes[node.parentID]
			p := &pools[node.poolIdx]
			revSteps = append(revSteps, RouteStep{
				Pool:     p.Address,
				PoolType: p.PoolType,
				TokenIn:  parent.token,
				TokenOut: node.token,
			})
			poolAddrs = append(poolAddrs, p.Address)
			poolTypes = append(poolTypes, p.PoolType)
			extraDatas = append(extraDatas, p.ExtraData)
			tokenPairs = append(tokenPairs, node.tokenIn, node.token)
			idx = node.parentID
		}

		// Reverse all slices
		for i, j := 0, len(revSteps)-1; i < j; i, j = i+1, j-1 {
			revSteps[i], revSteps[j] = revSteps[j], revSteps[i]
			poolAddrs[i], poolAddrs[j] = poolAddrs[j], poolAddrs[i]
			poolTypes[i], poolTypes[j] = poolTypes[j], poolTypes[i]
			extraDatas[i], extraDatas[j] = extraDatas[j], extraDatas[i]
			tokenPairs[i*2], tokenPairs[j*2] = tokenPairs[j*2], tokenPairs[i*2]
			tokenPairs[i*2+1], tokenPairs[j*2+1] = tokenPairs[j*2+1], tokenPairs[i*2+1]
		}

		return revSteps, poolAddrs, poolTypes, extraDatas
	}

	// ── EVM verification of top 5 ───────────────────────────────────

	evmCtx := statedb.GetCachedContext(cfg)

	top := 5
	if top > len(candidates) {
		top = len(candidates)
	}

	var bestRoute *Route
	evmQuotes := 0
	for _, cand := range candidates[:top] {
		steps, poolAddrs, poolTypes, extraDatas := backtrack(cand.nodeIdx)

		// Build swap() calldata — full chain, single EVM call
		tokenPairs := make([]common.Address, len(steps)*2)
		for i, s := range steps {
			tokenPairs[i*2] = s.TokenIn
			tokenPairs[i*2+1] = s.TokenOut
		}
		calldata := EncodeSwapMulti(poolAddrs, poolTypes, tokenPairs, amountIn, extraDatas, uint256.NewInt(0))

		evmQuotes++
		cs := statedb.NewCallState(stateWithOverrides)
		ret, gasUsed, err := evmCtx.ExecuteWithCallState(cs, sender, routerAddr, calldata)
		if err != nil || len(ret) < 32 {
			continue
		}
		evmAmount := new(uint256.Int)
		evmAmount.SetBytes(ret[:32])
		if evmAmount.IsZero() {
			continue
		}

		if bestRoute == nil || evmAmount.Gt(bestRoute.AmountOut) {
			bestRoute = &Route{
				Steps:     steps,
				AmountOut: new(uint256.Int).Set(evmAmount),
				GasUsed:   gasUsed,
				Calldata:  calldata,
				Stats: RouteStats{
					FormulaQuotes: formulaQuotes,
					EVMQuotes:     evmQuotes,
					TotalQuotes:   formulaQuotes + evmQuotes,
				},
			}
		}
	}

	if bestRoute == nil {
		return nil
	}
	return bestRoute
}

// ── Helpers ──────────────────────────────────────────────────────────

// ParsedOverride holds pre-parsed override data.
type ParsedOverride struct {
	Addr    common.Address
	Balance *uint256.Int
	Nonce   uint64
	Code    []byte
	Slots   []struct {
		Slot  common.Hash
		Value common.Hash
	}
}

// ApplyOverrides creates a fresh overlay with overrides applied.
func ApplyOverrides(base *statedb.StateDB, overrides []ParsedOverride) *statedb.StateDB {
	if len(overrides) == 0 {
		return base
	}
	overlay := base.NewOverlay()
	for _, po := range overrides {
		if po.Code != nil {
			overlay.SetAccount(po.Addr, po.Balance, po.Nonce, po.Code)
		}
		for _, s := range po.Slots {
			overlay.SetStorageSlot(po.Addr, s.Slot, s.Value)
		}
	}
	return overlay
}

// ApplyOverridesFlat creates an overlay of base with overrides baked in.
func ApplyOverridesFlat(base *statedb.StateDB, overrides []ParsedOverride) *statedb.StateDB {
	if len(overrides) == 0 {
		return base
	}
	overlay := base.NewOverlay()
	for _, po := range overrides {
		if po.Code != nil {
			overlay.SetAccount(po.Addr, po.Balance, po.Nonce, po.Code)
		}
		for _, s := range po.Slots {
			overlay.SetStorageSlot(po.Addr, s.Slot, s.Value)
		}
	}
	return overlay
}
