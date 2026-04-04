package splitter

import "github.com/holiman/uint256"

// Split runs the recommended production strategy: GreedyDynamic with 8% discovery
// and 2% fine-tune chunks. Zero losses across all tested conditions, ~210ms median.
//
// See README.md in this package for strategy comparison and benchmarks.
func Split(p *Params, amountIn *uint256.Int) *Result {
	return GreedyDynamic(p, amountIn, 8, 2)
}
