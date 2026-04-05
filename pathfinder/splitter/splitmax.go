package splitter

import "github.com/holiman/uint256"

// SplitMax runs three strategies and returns the best result.
//
// 1. GreedyCompete(30,2) — safe floor, never loses to single path
// 2. GreedyMixed(grad) — smooth taper, most wins historically
// 3. GreedyMixed(shuf2) — interleaved 10/5/2 pattern, different path diversity
//
// The second and third strategies benefit from the warm quote cache the first
// one built. Cost: ~1s total (less than 3× a single strategy due to cache sharing).
func SplitMax(p *Params, amountIn *uint256.Int) *Result {
	results := []*Result{
		GreedyCompete(p, amountIn, 30, 2),
		GreedyMixed(p, amountIn, SchedGradual),
		GreedyMixed(p, amountIn, SchedShuffle2),
	}

	var best *Result
	for _, r := range results {
		if r == nil {
			continue
		}
		if best == nil || r.Total.Gt(&best.Total) {
			best = r
		}
	}
	return best
}
