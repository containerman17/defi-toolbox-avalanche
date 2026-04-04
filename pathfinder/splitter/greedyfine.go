package splitter

import "github.com/holiman/uint256"

// GreedyFine is Greedy with 4x more chunks for finer-grained allocation.
//
// More chunks = smaller volume per chunk = less price impact per chunk =
// better output. Benchmarked across 17 token pairs at ~$1M volumes:
//   - GreedyFine (40 chunks) beats Greedy (10 chunks) on all 17 pairs
//   - Median improvement: +0.05% over Greedy (varies by pair, up to +45%)
//   - Cost: ~3x slower (linear in chunk count)
//
// Diminishing returns beyond 4x: 80 chunks adds <$5 on a $1M swap.
func GreedyFine(p *Params, amountIn *uint256.Int, chunks int) *Result {
	return Greedy(p, amountIn, chunks*4)
}
