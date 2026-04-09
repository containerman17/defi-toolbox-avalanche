package splitter

import (
	"time"

	"github.com/holiman/uint256"
)

// Best runs all strategies in sequence and returns the one with the highest
// total output. Use when compute time is not a constraint and you want the
// absolute best result.
func Best(p *Params, amountIn *uint256.Int, chunks int) *Result {
	t0 := time.Now()

	strategies := []struct {
		name string
		run  func() *Result
	}{
		{"greedy", func() *Result { return Greedy(p, amountIn, chunks) }},
		{"optimized", func() *Result { return Optimized(p, amountIn, chunks) }},
		{"greedyfine", func() *Result { return GreedyFine(p, amountIn, chunks) }},
		{"greedy8x", func() *Result { return Greedy(p, amountIn, chunks*8) }},
	}

	var best *Result
	for _, s := range strategies {
		r := s.run()
		if r == nil {
			continue
		}
		if best == nil || r.Total.Gt(&best.Total) {
			best = r
		}
	}

	if best != nil {
		best.ElapsedUs = time.Since(t0).Microseconds()
	}
	return best
}
