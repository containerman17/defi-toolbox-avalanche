package splitter

import "github.com/holiman/uint256"

// Split runs the recommended production strategy: GreedyCompete with 30% slabs
// and 2% chunks. Provably never returns less than single-path (min = +0.000%).
// 43 wins, 0 losses across 57 test cases. ~530ms median.
//
// See README.md in this package for strategy comparison and benchmarks.
func Split(p *Params, amountIn *uint256.Int) *Result {
	return GreedyCompete(p, amountIn, 30, 2)
}
