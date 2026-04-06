package splitter

import "github.com/holiman/uint256"

// SplitMax runs four complementary strategies, collects all discovered paths,
// then re-allocates across the combined path set for the best result.
//
// 1. GreedyMixed(front) — front-loaded schedule, most near-best hits
// 2. OptimizedV2 — shuf2 discovery + fine reallocation, covers front's misses
// 3. GreedyMixed(grad) — smooth taper, finds paths the above two miss
// 4. GreedyCompete(30,2) — tournament split, covers remaining edge cases
// 5. AllocateAcrossPaths on the union of all paths from (1)-(4)
//
// Returns the best of all five results. 358/360 near-best at ~236ms across
// 10 blocks. Strategies share the warm quote cache.
func SplitMax(p *Params, amountIn *uint256.Int) *Result {
	underlying := []*Result{
		GreedyMixed(p, amountIn, SchedFrontLoaded),
		OptimizedV2(p, amountIn),
		GreedyMixed(p, amountIn, SchedGradual),
		GreedyCompete(p, amountIn, 30, 2),
	}

	var best *Result
	var allLegs []Leg
	for _, r := range underlying {
		if r == nil {
			continue
		}
		allLegs = append(allLegs, r.Legs...)
		if best == nil || r.Total.Gt(&best.Total) {
			best = r
		}
	}
	if best == nil {
		return nil
	}

	paths := CollectPaths(allLegs)
	phase2 := AllocateAcrossPaths(p, amountIn, paths)
	if phase2 != nil && phase2.Total.Gt(&best.Total) {
		best = phase2
	}

	return best
}
