package splitter

import "github.com/holiman/uint256"

// Split runs the recommended production strategy: SplitMax.
// Best of Greedy(10) + GreedyCompete(30,2) + GreedyMixed(grad) + GreedyMixed(shuf2).
// Four strategies in ~230ms thanks to formula quote cache sharing — the first strategy
// warms the cache, subsequent strategies get near-free formula lookups and only pay
// for EVM verification. Faster than running 100 chunks of a single strategy (~1s).
//
// See README.md in this package for strategy comparison and benchmarks.
func Split(p *Params, amountIn *uint256.Int) *Result {
	return SplitMax(p, amountIn)
}
