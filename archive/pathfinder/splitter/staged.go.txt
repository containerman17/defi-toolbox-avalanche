package splitter

import (
	"time"

	"github.com/holiman/uint256"
)

// Staged runs the same underlying strategies as SplitMax (front + optim2),
// collects all discovered paths from both, then re-allocates the full amount
// across the combined path set with fine 2% chunks.
//
// Returns the best of (front, optim2, re-allocation). Guaranteed >= SplitMax
// since it considers the same candidates plus the re-allocation. The
// re-allocation can win because it operates on a superset of paths from both
// strategies and uses finer allocation granularity.
func Staged(p *Params, amountIn *uint256.Int) *Result {
	t0 := time.Now()

	front := GreedyMixed(p, amountIn, SchedFrontLoaded)
	optim2 := OptimizedV2(p, amountIn)

	// Best of the two underlying strategies (= what SplitMax returns)
	best := front
	if optim2 != nil && (best == nil || optim2.Total.Gt(&best.Total)) {
		best = optim2
	}
	if best == nil {
		return nil
	}

	// Collect union of all paths from both strategies
	var allLegs []Leg
	if front != nil {
		allLegs = append(allLegs, front.Legs...)
	}
	if optim2 != nil {
		allLegs = append(allLegs, optim2.Legs...)
	}
	paths := CollectPaths(allLegs)

	// Re-allocate across the combined path set
	phase2 := AllocateAcrossPaths(p, amountIn, paths)
	if phase2 != nil && phase2.Total.Gt(&best.Total) {
		best = phase2
	}

	best.ElapsedUs = time.Since(t0).Microseconds()
	return best
}
