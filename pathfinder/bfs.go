package pathfinder

import (
	"defi-toolbox/formulas"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// RouteStep represents one hop in a route.
type RouteStep struct {
	Pool      common.Address `json:"pool"`
	PoolType  int            `json:"poolType"`
	TokenIn   common.Address `json:"tokenIn"`
	TokenOut  common.Address `json:"tokenOut"`
	ExtraData string         `json:"extraData,omitempty"`
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
	FormulaQuotes int `json:"formulaQuotes"`
	EVMQuotes     int `json:"evmQuotes"`
	TotalQuotes   int `json:"totalQuotes"`
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

// FormulaSearchOptions controls formula-only BFS behavior.
type FormulaSearchOptions struct {
	BeamWidth int
	Blacklist map[uint16]bool
}

// FindBestFormulaRoute returns the single best formula-only route.
func FindBestFormulaRoute(
	pm formulas.PoolQuoterSource,
	adj map[common.Address][]PoolEdge,
	pools []Pool,
	tokenIn, tokenOut common.Address,
	amountIn *uint256.Int,
	maxHops int,
	opts FormulaSearchOptions,
) *Route {
	routes := FindTopFormulaRoutes(pm, adj, pools, tokenIn, tokenOut, amountIn, maxHops, 1, opts)
	if len(routes) == 0 {
		return nil
	}
	return routes[0]
}

// FindTopFormulaRoutes finds the top N swap routes using formulas only.
func FindTopFormulaRoutes(
	pm formulas.PoolQuoterSource,
	adj map[common.Address][]PoolEdge,
	pools []Pool,
	tokenIn, tokenOut common.Address,
	amountIn *uint256.Int,
	maxHops int,
	limit int,
	opts FormulaSearchOptions,
) []*Route {
	if amountIn.IsZero() {
		return nil
	}
	if limit <= 0 {
		limit = 1
	}
	cyclic := tokenIn == tokenOut
	if maxHops <= 0 || maxHops > 4 {
		maxHops = 4
	}
	beamWidth := opts.BeamWidth
	if beamWidth <= 0 {
		beamWidth = topK
	}

	allNodes := make([]bfsNode, 1, 1024)
	allNodes[0] = bfsNode{
		amount:   *amountIn,
		parentID: -1,
		token:    tokenIn,
	}

	currentLayer := []int32{0}
	formulaQuotes := 0

	type candidate struct {
		nodeIdx int32
	}
	var candidates []candidate

	for hop := 0; hop < maxHops; hop++ {
		type topEntry struct {
			amount  uint256.Int
			nodeIdx int32
		}
		tokenBest := make(map[common.Address][]topEntry)

		for _, parentIdx := range currentLayer {
			parent := &allNodes[parentIdx]

			for _, edge := range adj[parent.token] {
				if opts.Blacklist != nil && opts.Blacklist[edge.PoolIdx] {
					continue
				}
				if parent.parentID >= 0 && edge.PoolIdx == parent.poolIdx {
					continue
				}
				if !cyclic && edge.TokenOut == tokenIn {
					continue
				}

				formulaQuotes++
				out := pm.Quote(pools[edge.PoolIdx].Address, &parent.amount, edge.TokenIn, edge.TokenOut)
				if out.IsZero() {
					continue
				}

				tok := edge.TokenOut
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
				if len(entries) < beamWidth {
					nodeIdx := int32(len(allNodes))
					allNodes = append(allNodes, bfsNode{
						amount:   out,
						parentID: parentIdx,
						poolIdx:  edge.PoolIdx,
						tokenIn:  edge.TokenIn,
						token:    tok,
					})
					tokenBest[tok] = append(entries, topEntry{amount: out, nodeIdx: nodeIdx})
					continue
				}

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

		if !cyclic {
			if entries, ok := tokenBest[tokenOut]; ok {
				for _, e := range entries {
					candidates = append(candidates, candidate{nodeIdx: e.nodeIdx})
				}
			}
		}

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

	backtrack := func(nodeIdx int32) []RouteStep {
		var revSteps []RouteStep
		idx := nodeIdx
		for idx >= 0 && allNodes[idx].parentID >= 0 {
			node := &allNodes[idx]
			parent := &allNodes[node.parentID]
			p := &pools[node.poolIdx]
			revSteps = append(revSteps, RouteStep{
				Pool:      p.Address,
				PoolType:  p.PoolType,
				TokenIn:   parent.token,
				TokenOut:  node.token,
				ExtraData: p.ExtraData,
			})
			idx = node.parentID
		}
		for i, j := 0, len(revSteps)-1; i < j; i, j = i+1, j-1 {
			revSteps[i], revSteps[j] = revSteps[j], revSteps[i]
		}
		return revSteps
	}

	routes := make([]*Route, 0, limit)
	for _, cand := range candidates {
		routes = append(routes, &Route{
			Steps:     backtrack(cand.nodeIdx),
			AmountOut: new(uint256.Int).Set(&allNodes[cand.nodeIdx].amount),
			Stats: RouteStats{
				FormulaQuotes: formulaQuotes,
				TotalQuotes:   formulaQuotes,
			},
		})
		if len(routes) >= limit {
			break
		}
	}
	return routes
}

// QuotePath formula-quotes a specific multi-hop path at a given volume.
// Chains pm.Quote calls along the steps. Returns zero if any hop fails.
func QuotePath(pm formulas.PoolQuoterSource, steps []RouteStep, amountIn *uint256.Int) uint256.Int {
	current := *amountIn
	for _, s := range steps {
		out := pm.Quote(s.Pool, &current, s.TokenIn, s.TokenOut)
		if out.IsZero() {
			return out
		}
		current = out
	}
	return current
}
