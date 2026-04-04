package pathfinder

import "github.com/holiman/uint256"

// mergeNode is a node in a suffix trie built from reversed route paths.
// Children represent steps further from the end of the original path (i.e.,
// closer to the start / the feeder steps). Leaf nodes carry volumes.
type mergeNode struct {
	step     RouteStep
	key      string // pool+tokenIn+tokenOut — identity for trie branching
	children []*mergeNode
	volumes  []*uint256.Int // non-empty only at leaves (path starts)
}

// stepMergeKey returns a string that uniquely identifies a step for merging.
// Two steps with the same pool, tokenIn, and tokenOut are considered identical.
func stepMergeKey(s RouteStep) string {
	var buf [60]byte // 20+20+20
	copy(buf[0:20], s.Pool[:])
	copy(buf[20:40], s.TokenIn[:])
	copy(buf[40:60], s.TokenOut[:])
	return string(buf[:])
}

// MergeRoutes takes a set of split-routing legs and produces a minimal
// sequence of steps by identifying shared suffixes across routes.
//
// Phase 1 — Suffix trie: builds a trie from reversed paths.
//   - Identical paths collapse into one leaf (volumes summed)
//   - Shared suffixes become shared internal nodes (amount=0, uses balanceOf)
//   - Feeder steps (unique prefixes) keep their explicit volumes
//
// Phase 2 — First-hop merging: after suffix emission, groups branches
// that share the same first step (same pool+tokenIn+tokenOut). Within
// each group, the shared first step is called once with total volume.
// Consumers get explicit intermediate amounts for all but the last,
// which uses amount=0 (balance) to sweep leftovers.
//
// First-hop merging requires intermediate output estimates. If a Quoter
// is provided to MergeRoutesWithQuoter, it formula-quotes the shared
// first hop to compute intermediate amounts. Without a quoter,
// MergeRoutes only does suffix merging.
func MergeRoutes(routes []SquishRoute) ([]RouteStep, []*uint256.Int) {
	return mergeRoutesInner(routes, nil)
}

// MergeRoutesWithQuoter is like MergeRoutes but also merges shared first
// hops across branches. The quoter function takes a pool step and input
// amount, returns the expected output amount (formula quote). This is used
// to compute explicit intermediate amounts for consumers.
func MergeRoutesWithQuoter(routes []SquishRoute, quoter func(step RouteStep, amountIn *uint256.Int) uint256.Int) ([]RouteStep, []*uint256.Int) {
	return mergeRoutesInner(routes, quoter)
}

func mergeRoutesInner(routes []SquishRoute, quoter func(RouteStep, *uint256.Int) uint256.Int) ([]RouteStep, []*uint256.Int) {
	if len(routes) == 0 {
		return nil, nil
	}
	if len(routes) == 1 {
		steps := make([]RouteStep, len(routes[0].Steps))
		amounts := make([]*uint256.Int, len(routes[0].Steps))
		copy(steps, routes[0].Steps)
		amounts[0] = new(uint256.Int).Set(routes[0].Volume)
		for i := 1; i < len(amounts); i++ {
			amounts[i] = uint256.NewInt(0)
		}
		return steps, amounts
	}

	root := &mergeNode{}

	// Build suffix trie: insert each route reversed (last step first).
	for _, r := range routes {
		if len(r.Steps) == 0 {
			continue
		}
		cur := root
		for i := len(r.Steps) - 1; i >= 0; i-- {
			step := r.Steps[i]
			key := stepMergeKey(step)
			var found *mergeNode
			for _, ch := range cur.children {
				if ch.key == key {
					found = ch
					break
				}
			}
			if found == nil {
				found = &mergeNode{step: step, key: key}
				cur.children = append(cur.children, found)
			}
			cur = found
		}
		cur.volumes = append(cur.volumes, new(uint256.Int).Set(r.Volume))
	}

	// Extract mixed nodes (both volumes and children).
	var overflow []SquishRoute
	extractMixed(root, &overflow)

	// Phase 1: DFS post-order (suffix merging).
	var steps []RouteStep
	var amounts []*uint256.Int
	for _, ch := range root.children {
		emitDFS(ch, &steps, &amounts)
	}

	// Append overflow.
	for _, r := range overflow {
		for i, s := range r.Steps {
			steps = append(steps, s)
			if i == 0 {
				amounts = append(amounts, new(uint256.Int).Set(r.Volume))
			} else {
				amounts = append(amounts, uint256.NewInt(0))
			}
		}
	}

	// Phase 2: First-hop merging (requires quoter).
	if quoter != nil {
		steps, amounts = mergeFirstHops(steps, amounts, quoter)
	}

	return steps, amounts
}

// branch is a contiguous sequence of steps from the phase-1 output that
// forms one logical route (explicit first-step amount, followed by zeros).
type branch struct {
	startIdx int
	endIdx   int // exclusive
}

// mergeFirstHops identifies groups of branches that share the same first
// step and merges them: one call to the shared pool with total volume,
// then each consumer tail with explicit intermediate amounts (last gets 0).
func mergeFirstHops(steps []RouteStep, amounts []*uint256.Int, quoter func(RouteStep, *uint256.Int) uint256.Int) ([]RouteStep, []*uint256.Int) {
	// Split the flat step list into branches.
	// A new branch starts at each step with a non-zero amount.
	var branches []branch
	for i := range steps {
		if !amounts[i].IsZero() {
			if len(branches) > 0 {
				branches[len(branches)-1].endIdx = i
			}
			branches = append(branches, branch{startIdx: i})
		}
	}
	if len(branches) > 0 {
		branches[len(branches)-1].endIdx = len(steps)
	}

	if len(branches) < 2 {
		return steps, amounts
	}

	// Group branches by first step key.
	type branchGroup struct {
		key     string
		step    RouteStep
		indices []int // indices into branches slice
	}
	groupMap := make(map[string]*branchGroup)
	var groupOrder []string
	for i, b := range branches {
		key := stepMergeKey(steps[b.startIdx])
		if g, ok := groupMap[key]; ok {
			g.indices = append(g.indices, i)
		} else {
			groupMap[key] = &branchGroup{
				key:     key,
				step:    steps[b.startIdx],
				indices: []int{i},
			}
			groupOrder = append(groupOrder, key)
		}
	}

	// Check if any group has >1 branch (otherwise no first-hop merging possible).
	anyMergeable := false
	for _, key := range groupOrder {
		if len(groupMap[key].indices) > 1 {
			// Only merge if all branches in this group have >1 step
			// (single-step branches are already optimal)
			allMultiStep := true
			for _, bi := range groupMap[key].indices {
				b := branches[bi]
				if b.endIdx-b.startIdx < 2 {
					allMultiStep = false
					break
				}
			}
			if allMultiStep {
				anyMergeable = true
				break
			}
		}
	}
	if !anyMergeable {
		return steps, amounts
	}

	// Rebuild: for each group, emit merged first hop + tails with explicit amounts.
	var outSteps []RouteStep
	var outAmounts []*uint256.Int

	for _, key := range groupOrder {
		g := groupMap[key]

		if len(g.indices) < 2 {
			// Single branch — emit as-is.
			for _, bi := range g.indices {
				b := branches[bi]
				for j := b.startIdx; j < b.endIdx; j++ {
					outSteps = append(outSteps, steps[j])
					outAmounts = append(outAmounts, amounts[j])
				}
			}
			continue
		}

		// Check all branches have >1 step.
		allMultiStep := true
		for _, bi := range g.indices {
			b := branches[bi]
			if b.endIdx-b.startIdx < 2 {
				allMultiStep = false
				break
			}
		}
		if !allMultiStep {
			// Can't merge — emit each branch as-is.
			for _, bi := range g.indices {
				b := branches[bi]
				for j := b.startIdx; j < b.endIdx; j++ {
					outSteps = append(outSteps, steps[j])
					outAmounts = append(outAmounts, amounts[j])
				}
			}
			continue
		}

		// Merge: emit shared first step with total volume.
		totalVolume := new(uint256.Int)
		for _, bi := range g.indices {
			totalVolume.Add(totalVolume, amounts[branches[bi].startIdx])
		}
		outSteps = append(outSteps, g.step)
		outAmounts = append(outAmounts, totalVolume)

		// Emit each branch's tail (steps after the first hop).
		// All but the last get explicit intermediate amounts from the quoter.
		// The last gets amount=0 (balance sweep).
		for idx, bi := range g.indices {
			b := branches[bi]
			isLast := idx == len(g.indices)-1

			for j := b.startIdx + 1; j < b.endIdx; j++ {
				outSteps = append(outSteps, steps[j])
				if j == b.startIdx+1 && !isLast {
					// First step of tail — set explicit intermediate amount.
					branchVolume := amounts[b.startIdx]
					intermediateOut := quoter(g.step, branchVolume)
					outAmounts = append(outAmounts, new(uint256.Int).Set(&intermediateOut))
				} else {
					outAmounts = append(outAmounts, amounts[j])
				}
			}
		}
	}

	return outSteps, outAmounts
}

// emitDFS does a post-order DFS: emit all children (feeders) first, then
// this node. Leaf nodes get their summed volume; internal nodes get amount=0.
func emitDFS(node *mergeNode, steps *[]RouteStep, amounts *[]*uint256.Int) {
	for _, ch := range node.children {
		emitDFS(ch, steps, amounts)
	}

	*steps = append(*steps, node.step)

	if len(node.volumes) > 0 {
		total := new(uint256.Int)
		for _, v := range node.volumes {
			total.Add(total, v)
		}
		*amounts = append(*amounts, total)
	} else {
		*amounts = append(*amounts, uint256.NewInt(0))
	}
}

// extractMixed finds nodes that have BOTH volumes and children.
func extractMixed(node *mergeNode, out *[]SquishRoute) {
	for _, ch := range node.children {
		extractMixed(ch, out)
		if len(ch.volumes) > 0 && len(ch.children) > 0 {
			for _, v := range ch.volumes {
				*out = append(*out, SquishRoute{
					Steps:  rebuildPath(ch),
					Volume: v,
				})
			}
			ch.volumes = nil
		}
	}
}

func rebuildPath(node *mergeNode) []RouteStep {
	return []RouteStep{node.step}
}
