package splitter

import "github.com/holiman/uint256"

// Split runs the recommended production strategy: SplitMax.
// Four strategies (front + optim2 + grad + c30_2) plus a reallocation pass
// across the combined path set. 358/360 near-best at ~236ms across 10 blocks.
// Cache sharing makes each additional strategy nearly free after the first.
//
// This is NOT the absolute best — adding GreedyFine reaches 360/360 at ~426ms.
// The default trades 2 edge cases for ~45% less latency.
func Split(p *Params, amountIn *uint256.Int) *Result {
	return SplitMax(p, amountIn)
}
