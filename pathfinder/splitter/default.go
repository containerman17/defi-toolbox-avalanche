package splitter

import "github.com/holiman/uint256"

// Split runs the recommended production strategy: SplitMax.
// Best of GreedyCompete(30,2) + GreedyMixed(grad) + GreedyMixed(shuf2).
// 282 wins, 5 losses (sub-wei), min=-0.000% across 399 deterministic test cases.
// ~240ms thanks to cache sharing between strategies.
//
// See README.md in this package for strategy comparison and benchmarks.
func Split(p *Params, amountIn *uint256.Int) *Result {
	return SplitMax(p, amountIn)
}
