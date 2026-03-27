package arb

import (
	"defi-toolbox/formulas"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// Opportunity represents a profitable cyclic arbitrage found by the scanner.
type Opportunity struct {
	Cycle         *Cycle
	SizeBucket    int
	AmountIn      *uint256.Int
	FormulaOut    *uint256.Int
	EVMOut        *uint256.Int
	EVMGasUsed    uint64
	FormulaProfit float64
	EVMProfit     float64
	EVMVerified   bool
}

// Scanner implements the 3-phase pipeline: rate screening → formula quoting → EVM verification.
type Scanner struct {
	cycles       []Cycle
	rates        *RateTable
	pm           *formulas.PoolManager
	pt           *PoolTable
	cyclesByPool map[uint16][]int32 // poolIdx → cycle indices (int32 saves memory at >1M cycles)
	hub          common.Address
	MaxSize      *uint256.Int // max trade size (set from wallet balance)
}

func NewScanner(cycles []Cycle, pm *formulas.PoolManager, pt *PoolTable, hub common.Address) *Scanner {
	s := &Scanner{
		cycles:       cycles,
		rates:        NewRateTable(),
		pm:           pm,
		pt:           pt,
		cyclesByPool: make(map[uint16][]int32),
		hub:          hub,
	}

	// Build reverse index: poolIdx → cycle indices
	for i := range cycles {
		var seen [4]uint16
		nSeen := 0
		for j := 0; j < int(cycles[i].Hops); j++ {
			pid := cycles[i].Pools[j]
			dup := false
			for k := 0; k < nSeen; k++ {
				if seen[k] == pid {
					dup = true
					break
				}
			}
			if !dup {
				seen[nSeen] = pid
				nSeen++
				s.cyclesByPool[pid] = append(s.cyclesByPool[pid], int32(i))
			}
		}
	}

	return s
}

func (s *Scanner) PoolTable() *PoolTable { return s.pt }
func (s *Scanner) RateTable() *RateTable { return s.rates }
func (s *Scanner) CycleCount() int       { return len(s.cycles) }

// InitRates does a full rate table sweep (all pools are dirty on startup).
func (s *Scanner) InitRates() {
	poolSet := make(map[uint16]bool)
	for i := range s.cycles {
		for j := 0; j < int(s.cycles[i].Hops); j++ {
			poolSet[s.cycles[i].Pools[j]] = true
		}
	}
	ok := 0
	for pid := range poolSet {
		addr := s.pt.Addr(pid)
		if s.rates.Update(addr, s.pm) {
			ok++
		}
	}
	fmt.Fprintf(os.Stderr, "[arb] initial rate sweep: %d/%d pools quoted\n", ok, len(poolSet))
}

type candidate struct {
	cycleIdx int
	size     int
	product  float64
}

func (s *Scanner) OnBlock(
	dirtyPools []common.Address,
	evmVerifier *Verifier,
	gasPrice uint64,
) *Opportunity {
	if len(dirtyPools) == 0 {
		return nil
	}

	t0 := time.Now()

	// ── Phase 1: Update rates for dirty pools, screen cycles ──────
	for _, pool := range dirtyPools {
		s.rates.ClearDead(pool)
		s.rates.Update(pool, s.pm)
	}

	// Collect dirty cycle indices
	dirtyCycleSet := make(map[int]bool)
	for _, pool := range dirtyPools {
		if pid, ok := s.pt.Lookup(pool); ok {
			for _, idx := range s.cyclesByPool[pid] {
				dirtyCycleSet[int(idx)] = true
			}
		}
	}

	candidates := make([]candidate, 0, len(dirtyCycleSet)*NumSizeBuckets)
	for idx := range dirtyCycleSet {
		c := &s.cycles[idx]
		for size := 0; size < NumSizeBuckets; size++ {
			product := s.rates.ScreenCycle(c, s.pt, size)
			if product > 0.99 {
				candidates = append(candidates, candidate{cycleIdx: idx, size: size, product: product})
			}
		}
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].product > candidates[j].product })
	if len(candidates) > 500 {
		candidates = candidates[:500]
	}

	phase1Time := time.Since(t0)

	// ── Phase 2: Sequential formula quoting ──────────────────────
	// For each unique cycle in top candidates, try ALL size buckets to find optimal size.
	t1 := time.Now()
	type formulaResult struct {
		candidate
		amountIn  *uint256.Int
		amountOut *uint256.Int
		profit    float64
	}
	var results []formulaResult
	var nearMisses []formulaResult

	// Deduplicate cycles — try all sizes for each
	triedCycles := make(map[int]bool)
	for _, cand := range candidates {
		if triedCycles[cand.cycleIdx] {
			continue
		}
		triedCycles[cand.cycleIdx] = true

		c := &s.cycles[cand.cycleIdx]
		var bestFR *formulaResult
		var bestNM *formulaResult

		for size := 0; size < NumSizeBuckets; size++ {
			amountIn := SizeBuckets[size]
			if s.MaxSize != nil && amountIn.Cmp(s.MaxSize) > 0 {
				continue
			}
			out := s.sequentialQuote(c, amountIn)
			if out == nil || out.IsZero() {
				continue
			}

			inF := float64FromU256(amountIn)
			outF := float64FromU256(out)
			profitF := outF - inF

			fr := formulaResult{
				candidate: candidate{cycleIdx: cand.cycleIdx, size: size, product: cand.product},
				amountIn:  new(uint256.Int).Set(amountIn),
				amountOut: out,
				profit:    profitF,
			}

			if profitF > 0 {
				if bestFR == nil || profitF > bestFR.profit {
					bestFR = &fr
				}
			} else if inF > 0 && outF/inF > 0.99 {
				if bestNM == nil || profitF > bestNM.profit {
					bestNM = &fr
				}
			}
		}

		if bestFR != nil {
			results = append(results, *bestFR)
		}
		if bestNM != nil {
			nearMisses = append(nearMisses, *bestNM)
		}
	}

	sort.Slice(results, func(i, j int) bool { return results[i].profit > results[j].profit })
	sort.Slice(nearMisses, func(i, j int) bool { return nearMisses[i].profit > nearMisses[j].profit })

	phase2Time := time.Since(t1)

	// ── Phase 3: Time-budgeted EVM verification ──────────────────
	t2 := time.Now()
	evmBudget := 100 * time.Millisecond
	var bestOpp *Opportunity

	evmCandidates := append([]formulaResult{}, results...)
	nmCount := 3
	if nmCount > len(nearMisses) {
		nmCount = len(nearMisses)
	}
	evmCandidates = append(evmCandidates, nearMisses[:nmCount]...)

	if evmVerifier != nil && len(evmCandidates) > 0 {
		for _, r := range evmCandidates {
			if time.Since(t2) > evmBudget {
				break
			}

			c := &s.cycles[r.cycleIdx]
			evmOut, gasUsed, ok := evmVerifier.Verify(c, r.amountIn)
			if !ok || evmOut == nil {
				continue
			}

			// swap() returns gross amountOut. Profit = out - in - gasCost.
			gasCost := gasUsed * gasPrice
			gasCostU := new(uint256.Int).SetUint64(gasCost)
			totalCost := new(uint256.Int).Add(r.amountIn, gasCostU)
			var evmProfit float64
			if evmOut.Cmp(totalCost) > 0 {
				evmProfit = float64FromU256(new(uint256.Int).Sub(evmOut, totalCost))
			} else {
				evmProfit = -(float64FromU256(new(uint256.Int).Sub(totalCost, evmOut)))
			}

			if evmProfit > 0 && (bestOpp == nil || evmProfit > bestOpp.EVMProfit) {
				bestOpp = &Opportunity{
					Cycle:         c,
					SizeBucket:    r.size,
					AmountIn:      r.amountIn,
					FormulaOut:    r.amountOut,
					EVMOut:        evmOut,
					EVMGasUsed:    gasUsed,
					FormulaProfit: r.profit,
					EVMProfit:     evmProfit,
					EVMVerified:   true,
				}
			}
		}
	}

	phase3Time := time.Since(t2)

	if bestOpp == nil && len(results) > 0 {
		r := results[0]
		bestOpp = &Opportunity{
			Cycle: &s.cycles[r.cycleIdx], SizeBucket: r.size,
			AmountIn: r.amountIn, FormulaOut: r.amountOut, FormulaProfit: r.profit,
		}
	}
	if bestOpp == nil && len(nearMisses) > 0 {
		r := nearMisses[0]
		bestOpp = &Opportunity{
			Cycle: &s.cycles[r.cycleIdx], SizeBucket: r.size,
			AmountIn: r.amountIn, FormulaOut: r.amountOut, FormulaProfit: r.profit,
		}
	}

	totalTime := time.Since(t0)
	fmt.Fprintf(os.Stderr, "[arb] block: %d dirty pools, %d dirty cycles, %d screened, %d formula-profitable, %d near-misses | phase1=%v phase2=%v phase3=%v total=%v\n",
		len(dirtyPools), len(dirtyCycleSet), len(candidates), len(results), len(nearMisses),
		phase1Time.Round(time.Microsecond),
		phase2Time.Round(time.Microsecond),
		phase3Time.Round(time.Microsecond),
		totalTime.Round(time.Microsecond),
	)

	showNM := nearMisses
	if len(showNM) > 3 {
		showNM = showNM[:3]
	}
	sizeLabels := [NumSizeBuckets]string{"0.001", "0.01", "0.1", "1", "10"}
	for i, nm := range showNM {
		c := &s.cycles[nm.cycleIdx]
		inF := float64FromU256(nm.amountIn)
		ratio := float64FromU256(nm.amountOut) / inF
		lossBps := (1.0 - ratio) * 10000
		fmt.Fprintf(os.Stderr, "[arb]   near-miss #%d: %d-hop size=%s_AVAX ratio=%.6f loss=%.1fbps pools=[",
			i+1, c.Hops, sizeLabels[nm.size], ratio, lossBps)
		for j := 0; j < int(c.Hops); j++ {
			if j > 0 {
				fmt.Fprintf(os.Stderr, ",")
			}
			fmt.Fprintf(os.Stderr, "%s", s.pt.Addr(c.Pools[j]).Hex()[:10])
		}
		fmt.Fprintf(os.Stderr, "]\n")
	}

	return bestOpp
}

func (s *Scanner) sequentialQuote(c *Cycle, amountIn *uint256.Int) *uint256.Int {
	current := new(uint256.Int).Set(amountIn)
	for i := 0; i < int(c.Hops); i++ {
		out := s.pm.Quote(s.pt.Addr(c.Pools[i]), current, c.Dirs[i])
		if out.IsZero() {
			return nil
		}
		current = new(uint256.Int).Set(&out)
	}
	return current
}

func FormatOpportunity(opp *Opportunity, pt *PoolTable) string {
	if opp == nil {
		return "no opportunity"
	}

	inAvax := float64FromU256(opp.AmountIn) / 1e18
	formulaProfitAvax := opp.FormulaProfit / 1e18

	s := fmt.Sprintf("cycle=%d-hop in=%.4f_AVAX formula_profit=%.6f_AVAX",
		opp.Cycle.Hops, inAvax, formulaProfitAvax)

	if opp.EVMVerified {
		s += fmt.Sprintf(" evm_profit=%.6f_AVAX gas=%d", opp.EVMProfit/1e18, opp.EVMGasUsed)
	}

	s += " pools=["
	for i := 0; i < int(opp.Cycle.Hops); i++ {
		if i > 0 {
			s += ","
		}
		s += pt.Addr(opp.Cycle.Pools[i]).Hex()[:10]
	}
	s += "]"

	sizeLabels := [NumSizeBuckets]string{"0.001", "0.01", "0.1", "1", "10"}
	if opp.SizeBucket >= 0 && opp.SizeBucket < NumSizeBuckets {
		s += fmt.Sprintf(" size=%s_AVAX", sizeLabels[opp.SizeBucket])
	}

	return s
}
