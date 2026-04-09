// Deterministic split strategy benchmark.
// Runs all strategies across fixed blocks (deployment block + N*10000)
// using /debug/{block} frozen snapshots. Results are fully reproducible.
//
// Each (block × strategy) runs in its own goroutine with an isolated
// PoolManager — no cross-strategy cache sharing. Wall clock scales with cores.
package main

import (
	"flag"
	"fmt"
	"math"
	"math/big"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pf "defi-toolbox/pathfinder"
	"defi-toolbox/pathfinder/splitter"
	"defi-toolbox/quoter"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

const deployBlock = 82067033

var tokens = []struct {
	Name     string
	Address  common.Address
	Decimals int
	Amount   string // base amount (~$1M worth)
}{
	{"WAVAX", common.HexToAddress("0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7"), 18, "50000"},
	{"USDC", common.HexToAddress("0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E"), 6, "1000000"},
	{"USDT", common.HexToAddress("0x9702230A8Ea53601f5cD2dc00fDBc13d4dF4A8c7"), 6, "1000000"},
	{"WETH.e", common.HexToAddress("0x49D5c2BdFfac6CE2BFdB6640F4F80f226bc10bAB"), 18, "300"},
}

var dividers = []struct {
	label string
	div   uint64
}{
	{"1x", 1},
	{"÷10", 10},
	{"÷100", 100},
}

type strategy struct {
	name string
	run  func(*splitter.Params, *uint256.Int) *splitter.Result
}

// caseResult holds the result for one (block, strategy, pair, volume) combo.
type caseResult struct {
	pct    float64
	ms     float64
	result *splitter.Result
}

func main() {
	stateServer := flag.String("state-server", "ws://localhost:7449", "state server base URL (no /live)")
	numBlocks := flag.Int("blocks", 3, "number of blocks to test (at 10k intervals from deploy)")
	chunks := flag.Int("chunks", 10, "base number of chunks")
	only := flag.String("only", "", "comma-separated list of strategies to run (always includes greedy as baseline)")
	flag.Parse()

	ch := *chunks

	allStrategies := []strategy{
		{"greedy", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.Greedy(p, a, ch) }},
		{"optim", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.Optimized(p, a, ch) }},
		{"optim2", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.OptimizedV2(p, a) }},
		{"optim3", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.OptimizedV3(p, a) }},
		{"optim4", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.OptimizedV4(p, a) }},
		{"staged", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.Staged(p, a) }},
		{"gfine", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.GreedyFine(p, a, ch) }},
		{"gfast40", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.GreedyFast(p, a, ch*4) }},
		{"grad", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.GreedyMixed(p, a, splitter.SchedGradual) }},
		{"shuf2", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.GreedyMixed(p, a, splitter.SchedShuffle2) }},
		{"plat", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.GreedyMixed(p, a, splitter.SchedPlateau) }},
		{"front", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.GreedyMixed(p, a, splitter.SchedFrontLoaded) }},
		{"d8_2", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.GreedyDynamic(p, a, 8, 2) }},
		{"c30_2", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.GreedyCompete(p, a, 30, 2) }},
		{"c50_5", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.GreedyCompete(p, a, 50, 5) }},
		{"max", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.SplitMax(p, a) }},
	}

	var strategies []strategy
	if *only != "" {
		want := make(map[string]bool)
		want["greedy"] = true
		for _, name := range strings.Split(*only, ",") {
			want[strings.TrimSpace(name)] = true
		}
		for _, s := range allStrategies {
			if want[s.name] {
				strategies = append(strategies, s)
			}
		}
	} else {
		strategies = allStrategies
	}

	// Build test case list (pairs × volumes)
	type testCase struct {
		tIn, tOut int
		div       uint64
		divLabel  string
		pair      string
	}
	var cases []testCase
	for _, div := range dividers {
		for i := range tokens {
			for j := range tokens {
				pair := ""
				if i == j {
					pair = fmt.Sprintf("%s loop %s", tokens[i].Name, div.label)
				} else {
					pair = fmt.Sprintf("%s→%s %s", tokens[i].Name, tokens[j].Name, div.label)
				}
				cases = append(cases, testCase{i, j, div.div, div.label, pair})
			}
		}
	}

	nBlocks := *numBlocks
	nStrats := len(strategies)
	nCases := len(cases)
	totalJobs := nBlocks * nStrats * nCases
	// results[block][strategy][case]
	allResults := make([][][]caseResult, nBlocks)
	for bi := range allResults {
		allResults[bi] = make([][]caseResult, nStrats)
		for si := range allResults[bi] {
			allResults[bi][si] = make([]caseResult, nCases)
		}
	}

	// Connect to all blocks first
	type blockState struct {
		q   *quoter.Quoter
		ls  *statedb.LiveState
		cfg statedb.EVMConfig
	}
	blocks := make([]blockState, nBlocks)
	for bi := 0; bi < nBlocks; bi++ {
		blockNum := uint64(deployBlock + bi*10000)
		url := fmt.Sprintf("%s/debug/%d", strings.TrimRight(*stateServer, "/"), blockNum)
		fmt.Fprintf(os.Stderr, "connecting to block %d...\n", blockNum)
		ls, err := statedb.Connect(url)
		if err != nil {
			fmt.Fprintf(os.Stderr, "block %d: %v\n", blockNum, err)
			continue
		}
		q := quoter.NewQuoter(ls, 2000, 3)
		ls.RLock()
		blocks[bi] = blockState{q: q, ls: ls, cfg: ls.EVMConfig()}
	}

	// Progress tracker
	var completed int64
	go func() {
		for {
			time.Sleep(10 * time.Second)
			done := atomic.LoadInt64(&completed)
			if done >= int64(totalJobs) {
				return
			}
			fmt.Fprintf(os.Stderr, "progress: %d/%d jobs (%.0f%%)\n", done, totalJobs, float64(done)/float64(totalJobs)*100)
		}
	}()

	// Launch goroutine per (block × strategy) — each with its own fresh PM
	var wg sync.WaitGroup
	for bi := 0; bi < nBlocks; bi++ {
		bs := blocks[bi]
		if bs.q == nil {
			atomic.AddInt64(&completed, int64(nStrats*nCases))
			continue
		}
		for si := 0; si < nStrats; si++ {
			wg.Add(1)
			go func(bi, si int) {
				defer wg.Done()

				q := bs.q
				pm := q.NewPM()

				for ci, tc := range cases {
					tIn := tokens[tc.tIn]
					tOut := tokens[tc.tOut]

					amount := parseDecimalAmount(tIn.Amount, tIn.Decimals)
					if amount == nil || amount.Sign() <= 0 {
						continue
					}
					amount.Div(amount, big.NewInt(int64(tc.div)))
					if amount.Sign() <= 0 {
						continue
					}
					fullAmount := new(uint256.Int)
					fullAmount.SetFromBig(amount)

					params := &splitter.Params{
						PM: pm, BasePM: pm, Adj: q.Adj(), Pools: q.Pools(),
						State: q.StateWithOverrides(), EVMConfig: bs.cfg,
						RouterAddr: q.RouterAddr(), Sender: q.Sender(),
						TokenIn: tIn.Address, TokenOut: tOut.Address, MaxHops: q.MaxHops(),
					}

					// Single-path baseline (needed for pct calculation)
					single := pf.FindBestRoute(pm, params.Adj, params.Pools, params.State,
						bs.cfg, params.RouterAddr, params.Sender, tIn.Address, tOut.Address, fullAmount, q.MaxHops())
					if single == nil {
						continue
					}
					singleF := u256ToFloat(single.AmountOut, tOut.Decimals)

					result := strategies[si].run(params, fullAmount)
					var cr caseResult
					if result != nil {
						cr.result = result
						cr.pct = pctImprovement(singleF, u256ToFloat(&result.Total, tOut.Decimals))
						cr.ms = float64(result.ElapsedUs) / 1000.0
					}
					allResults[bi][si][ci] = cr
					atomic.AddInt64(&completed, 1)
				}
			}(bi, si)
		}
	}
	wg.Wait()
	fmt.Fprintf(os.Stderr, "done.\n")

	// Close connections
	for _, bs := range blocks {
		if bs.ls != nil {
			bs.ls.RUnlock()
			bs.ls.Close()
		}
	}

	// ── Print results ────────────────────────────────────────────────
	// Flatten into per-strategy arrays (same format as before)
	pcts := make([][]float64, nStrats)
	times := make([][]float64, nStrats)
	results := make([][]*splitter.Result, nStrats)
	for si := range strategies {
		pcts[si] = []float64{}
		times[si] = []float64{}
	}
	var pairLabels []string

	fmt.Printf("%-22s", "PAIR")
	for _, s := range strategies {
		fmt.Printf(" %8s", s.name)
	}
	fmt.Println()
	fmt.Println(strings.Repeat("─", 22+len(strategies)*9))

	for bi := 0; bi < nBlocks; bi++ {
		if blocks[bi].q == nil {
			continue
		}
		fmt.Printf("\n=== Block %d ===\n\n", uint64(deployBlock+bi*10000))

		for ci, tc := range cases {
			if bi == 0 {
				pairLabels = append(pairLabels, tc.pair)
			}
			fmt.Printf("%-22s", tc.pair)
			for si := range strategies {
				cr := allResults[bi][si][ci]
				pcts[si] = append(pcts[si], cr.pct)
				times[si] = append(times[si], cr.ms)
				results[si] = append(results[si], cr.result)
				fmt.Printf(" %+7.3f%%", cr.pct)
			}
			fmt.Println()
		}
	}

	// ── Aggregates ───────────────────────────────────────────────────
	n := len(pcts[0])
	fmt.Printf("\n%s\n", strings.Repeat("═", 22+len(strategies)*9))
	fmt.Printf("\nALL %d test cases (%d blocks × %d pairs × %d volumes):\n\n",
		n, nBlocks, len(tokens)*len(tokens), len(dividers))

	for _, label := range []string{"MEDIAN", "AVG", "MIN", "MAX"} {
		fmt.Printf("%-22s", label)
		for si := range strategies {
			sorted := make([]float64, len(pcts[si]))
			copy(sorted, pcts[si])
			sort.Float64s(sorted)
			var v float64
			switch label {
			case "MEDIAN":
				v = median(sorted)
			case "AVG":
				v = mean(pcts[si])
			case "MIN":
				v = sorted[0]
			case "MAX":
				v = sorted[len(sorted)-1]
			}
			fmt.Printf(" %+7.3f%%", v)
		}
		fmt.Println()
	}

	fmt.Println()
	fmt.Printf("%-22s", "AVG TIME")
	for si := range strategies {
		fmt.Printf(" %6.0fms", mean(times[si]))
	}
	fmt.Println()
	fmt.Printf("%-22s", "MED TIME")
	for si := range strategies {
		sorted := make([]float64, len(times[si]))
		copy(sorted, times[si])
		sort.Float64s(sorted)
		fmt.Printf(" %6.0fms", median(sorted))
	}
	fmt.Println()

	// Head-to-head vs greedy
	fmt.Println()
	fmt.Println("vs greedy (W/L/T):")
	for si := 1; si < len(strategies); si++ {
		wins, losses, t := 0, 0, 0
		for pi := range pcts[0] {
			if pcts[si][pi] > pcts[0][pi]+0.0001 {
				wins++
			} else if pcts[si][pi] < pcts[0][pi]-0.0001 {
				losses++
			} else {
				t++
			}
		}
		fmt.Printf("  %-8s W=%-3d L=%-3d T=%-3d\n", strategies[si].name, wins, losses, t)
	}

	// Per-case best
	const tol = 0.0001
	fmt.Println()
	fmt.Println("vs per-case best (W=near-best, L=missed, tol=0.0001%):")
	bestPct := make([]float64, n)
	for pi := range bestPct {
		for si := range strategies {
			if pcts[si][pi] > bestPct[pi] {
				bestPct[pi] = pcts[si][pi]
			}
		}
	}
	nearBest := make([][]bool, len(strategies))
	for si := range strategies {
		nearBest[si] = make([]bool, n)
		wins, losses := 0, 0
		for pi := range bestPct {
			if bestPct[pi]-pcts[si][pi] <= tol {
				nearBest[si][pi] = true
				wins++
			} else {
				losses++
			}
		}
		fmt.Printf("  %-8s near-best=%-3d missed=%-3d\n", strategies[si].name, wins, losses)
	}

	// Combinatorial search
	coverageOf := func(combo []int) int {
		count := 0
		for pi := 0; pi < n; pi++ {
			for _, si := range combo {
				if nearBest[si][pi] {
					count++
					break
				}
			}
		}
		return count
	}

	ns := len(strategies)
	for size := 2; size <= 4; size++ {
		bestCoverage := 0
		var bestCombos [][]int
		combo := make([]int, size)
		var search func(start, depth int)
		search = func(start, depth int) {
			if depth == size {
				c := coverageOf(combo)
				if c > bestCoverage {
					bestCoverage = c
					bestCombos = [][]int{append([]int(nil), combo...)}
				} else if c == bestCoverage {
					bestCombos = append(bestCombos, append([]int(nil), combo...))
				}
				return
			}
			for i := start; i <= ns-(size-depth); i++ {
				combo[depth] = i
				search(i+1, depth+1)
			}
		}
		search(0, 0)

		fmt.Printf("\nBest %d-strategy combo(s) covering %d/%d cases:\n", size, bestCoverage, n)
		shown := bestCombos
		if len(shown) > 5 {
			shown = shown[:5]
		}
		for _, c := range shown {
			names := make([]string, len(c))
			for i, si := range c {
				names[i] = strategies[si].name
			}
			fmt.Printf("  %v\n", names)
		}
		if len(bestCombos) > 5 {
			fmt.Printf("  ... and %d more\n", len(bestCombos)-5)
		}
	}
}

func legPaths(legs []splitter.Leg) []string {
	seen := make(map[string]bool)
	var paths []string
	for _, leg := range legs {
		var parts []string
		for _, s := range leg.Steps {
			parts = append(parts, s.Pool.Hex()[:10])
		}
		key := strings.Join(parts, "→")
		if !seen[key] {
			seen[key] = true
			paths = append(paths, key)
		}
	}
	return paths
}

func u256ToFloat(v *uint256.Int, decimals int) float64 {
	f, _ := new(big.Float).SetInt(v.ToBig()).Float64()
	return f / math.Pow10(decimals)
}

func pctImprovement(base, val float64) float64 {
	if base == 0 {
		return 0
	}
	return (val - base) / base * 100
}

func mean(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

func median(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 0 {
		return (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return sorted[n/2]
}

func parseDecimalAmount(s string, decimals int) *big.Int {
	parts := strings.SplitN(s, ".", 2)
	wholePart := parts[0]
	fracPart := ""
	if len(parts) == 2 {
		fracPart = parts[1]
	}
	if len(fracPart) > decimals {
		fracPart = fracPart[:decimals]
	} else {
		fracPart += strings.Repeat("0", decimals-len(fracPart))
	}
	combined := wholePart + fracPart
	result, ok := new(big.Int).SetString(combined, 10)
	if !ok {
		return nil
	}
	return result
}
