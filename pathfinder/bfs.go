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

// FormulaSearchOptions controls formula-only BFS behavior.
type FormulaSearchOptions struct {
	Blacklist map[uint16]bool
}

// FindBestFormulaRoute runs a beam-1 BFS over the formula graph and returns
// the single best route. Beam is hardcoded to 1 — DO NOT reintroduce
// configurable beam width. It leads to combinatorial explosion with
// non-linear timing and worse route discovery than the elimination strategy
// (run BFS once, then re-run with each pool in the best route blacklisted).
func FindBestFormulaRoute(
	pm formulas.PoolQuoterSource,
	adj map[common.Address][]PoolEdge,
	pools []Pool,
	tokenIn, tokenOut common.Address,
	amountIn *uint256.Int,
	maxHops int,
	opts FormulaSearchOptions,
) *Route {
	if amountIn.IsZero() {
		return nil
	}
	cyclic := tokenIn == tokenOut
	if maxHops <= 0 || maxHops > 4 {
		maxHops = 4
	}

	allNodes := make([]bfsNode, 1, 256)
	allNodes[0] = bfsNode{
		amount:   *amountIn,
		parentID: -1,
		token:    tokenIn,
	}

	currentLayer := []int32{0}
	formulaQuotes := 0

	// Best candidate reaching tokenOut so far.
	bestCandIdx := int32(-1)
	var bestCandAmount uint256.Int

	type bestEntry struct {
		amount  uint256.Int
		nodeIdx int32
	}

	for hop := 0; hop < maxHops; hop++ {
		// Beam=1: keep only the single best arrival per token.
		tokenBest := make(map[common.Address]bestEntry)

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

				// Cyclic candidate (arb route closed).
				if cyclic && tok == tokenOut && hop >= 1 {
					if out.Gt(&bestCandAmount) {
						bestCandIdx = int32(len(allNodes))
						bestCandAmount = out
						allNodes = append(allNodes, bfsNode{
							amount:   out,
							parentID: parentIdx,
							poolIdx:  edge.PoolIdx,
							tokenIn:  edge.TokenIn,
							token:    tok,
						})
					}
					continue
				}

				prev, exists := tokenBest[tok]
				if !exists || out.Gt(&prev.amount) {
					nodeIdx := int32(len(allNodes))
					allNodes = append(allNodes, bfsNode{
						amount:   out,
						parentID: parentIdx,
						poolIdx:  edge.PoolIdx,
						tokenIn:  edge.TokenIn,
						token:    tok,
					})
					tokenBest[tok] = bestEntry{amount: out, nodeIdx: nodeIdx}
				}
			}
		}

		// Non-cyclic: track best arrival at tokenOut.
		if !cyclic {
			if entry, ok := tokenBest[tokenOut]; ok {
				if entry.amount.Gt(&bestCandAmount) {
					bestCandIdx = entry.nodeIdx
					bestCandAmount = entry.amount
				}
			}
		}

		// Build next layer from all tokens except tokenOut.
		currentLayer = currentLayer[:0]
		for tok, entry := range tokenBest {
			if tok == tokenOut {
				continue
			}
			currentLayer = append(currentLayer, entry.nodeIdx)
		}
		if len(currentLayer) == 0 {
			break
		}
	}

	if bestCandIdx < 0 {
		return nil
	}

	// Backtrack to build route steps.
	var revSteps []RouteStep
	idx := bestCandIdx
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

	return &Route{
		Steps:     revSteps,
		AmountOut: new(uint256.Int).Set(&bestCandAmount),
		Stats: RouteStats{
			FormulaQuotes: formulaQuotes,
			TotalQuotes:   formulaQuotes,
		},
	}
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

// ── Elimination search ──────────────────────────────────────────────

// FindRoutesElimination discovers diverse routes via two strategies:
//
// 1. All-subsets: blacklist every non-empty subset of the best route's pools.
//    For [A, B] that's {A}, {B}, {A,B} — finds routes with partial and full avoidance.
//
// 2. Greedy disjoint: iteratively blacklist ALL pools from all previously found
//    routes, forcing each new route to use entirely different pools.
//
// Returns up to maxRoutes unique routes with no shared pools between them
// when possible.
func FindRoutesElimination(
	pm formulas.PoolQuoterSource,
	adj map[common.Address][]PoolEdge,
	pools []Pool,
	tokenIn, tokenOut common.Address,
	amountIn *uint256.Int,
	maxHops int,
	maxRoutes int,
) []*Route {
	best := FindBestFormulaRoute(pm, adj, pools, tokenIn, tokenOut, amountIn, maxHops, FormulaSearchOptions{})
	if best == nil {
		return nil
	}
	routes := []*Route{best}
	if maxRoutes <= 1 {
		return routes
	}

	// Phase 1: all subsets of the best route's pools.
	var bestPoolIdxs []uint16
	for _, step := range best.Steps {
		idx, ok := poolIdx(pools, step.Pool)
		if ok {
			bestPoolIdxs = append(bestPoolIdxs, idx)
		}
	}
	n := len(bestPoolIdxs)
	for mask := 1; mask < (1 << n); mask++ {
		if len(routes) >= maxRoutes {
			return routes
		}
		bl := make(map[uint16]bool)
		for bit := 0; bit < n; bit++ {
			if mask&(1<<bit) != 0 {
				bl[bestPoolIdxs[bit]] = true
			}
		}
		alt := FindBestFormulaRoute(pm, adj, pools, tokenIn, tokenOut, amountIn, maxHops,
			FormulaSearchOptions{Blacklist: bl})
		if alt != nil && !isDuplicateRoute(routes, alt) {
			routes = append(routes, alt)
		}
	}

	// Phase 2: greedy disjoint — blacklist ALL pools from all found routes,
	// keep finding new routes until maxRoutes or no more found.
	for len(routes) < maxRoutes {
		bl := make(map[uint16]bool)
		for _, r := range routes {
			for _, step := range r.Steps {
				if idx, ok := poolIdx(pools, step.Pool); ok {
					bl[idx] = true
				}
			}
		}
		alt := FindBestFormulaRoute(pm, adj, pools, tokenIn, tokenOut, amountIn, maxHops,
			FormulaSearchOptions{Blacklist: bl})
		if alt == nil || isDuplicateRoute(routes, alt) {
			break
		}
		routes = append(routes, alt)
	}

	return routes
}

func poolIdx(pools []Pool, addr common.Address) (uint16, bool) {
	for i := range pools {
		if pools[i].Address == addr {
			return uint16(i), true
		}
	}
	return 0, false
}

func isDuplicateRoute(routes []*Route, candidate *Route) bool {
	for _, r := range routes {
		if len(r.Steps) != len(candidate.Steps) {
			continue
		}
		same := true
		for i := range r.Steps {
			if r.Steps[i].Pool != candidate.Steps[i].Pool {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}

// ── Volume splitting ────────────────────────────────────────────────

// SplitResult holds the output of volume optimization across routes.
type SplitResult struct {
	Routes   []*Route
	Volumes  []*uint256.Int
	TotalOut *uint256.Int
}

// OptimalSplit distributes totalAmountIn across routes to maximize total output.
// Uses greedy rebalancing with formula quotes: moves volume from the route with
// worst marginal return to the one with best, halving the step until converged.
func OptimalSplit(pm formulas.PoolQuoterSource, routes []*Route, totalAmountIn *uint256.Int) *SplitResult {
	n := len(routes)
	if n == 0 {
		return nil
	}
	if n == 1 {
		out := QuotePath(pm, routes[0].Steps, totalAmountIn)
		return &SplitResult{
			Routes:   routes,
			Volumes:  []*uint256.Int{new(uint256.Int).Set(totalAmountIn)},
			TotalOut: new(uint256.Int).Set(&out),
		}
	}

	// Start with all volume on route 0 (the best single route).
	volumes := make([]*uint256.Int, n)
	volumes[0] = new(uint256.Int).Set(totalAmountIn)
	for i := 1; i < n; i++ {
		volumes[i] = new(uint256.Int)
	}

	// Step size starts at 10% of total, halves each round.
	step := new(uint256.Int).Div(totalAmountIn, uint256.NewInt(10))
	minStep := new(uint256.Int).Div(totalAmountIn, uint256.NewInt(10000))
	if minStep.IsZero() {
		minStep = uint256.NewInt(1)
	}

	for step.Gt(minStep) {
		improved := true
		for improved {
			improved = false
			for i := 0; i < n; i++ {
				for j := 0; j < n; j++ {
					if i == j || volumes[i].Lt(step) {
						continue
					}
					// Try moving step from route i to route j.
					before := evalSplit(pm, routes, volumes)
					volumes[i].Sub(volumes[i], step)
					volumes[j].Add(volumes[j], step)
					after := evalSplit(pm, routes, volumes)
					if after.Gt(&before) {
						improved = true
					} else {
						volumes[i].Add(volumes[i], step)
						volumes[j].Sub(volumes[j], step)
					}
				}
			}
		}
		step.Rsh(step, 1)
	}

	total := evalSplit(pm, routes, volumes)
	return &SplitResult{
		Routes:   routes,
		Volumes:  volumes,
		TotalOut: new(uint256.Int).Set(&total),
	}
}

func evalSplit(pm formulas.PoolQuoterSource, routes []*Route, volumes []*uint256.Int) uint256.Int {
	var total uint256.Int
	for i, route := range routes {
		if volumes[i].IsZero() {
			continue
		}
		out := QuotePath(pm, route.Steps, volumes[i])
		total.Add(&total, &out)
	}
	return total
}
