package splitter

import "github.com/holiman/uint256"

// SplitMax runs four strategies and returns the best result.
//
// 1. Greedy(10) — baseline, sometimes finds paths others miss at 10% volume
// 2. GreedyCompete(30,2) — safe floor, never loses to single path
// 3. GreedyMixed(grad) — smooth taper, most wins historically
// 4. GreedyMixed(shuf2) — interleaved 10/5/2 pattern, different path diversity
//
// Strategies share the warm quote cache. Cost: ~300ms total.
func SplitMax(p *Params, amountIn *uint256.Int) *Result {
	results := []*Result{
		Greedy(p, amountIn, 10),
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
