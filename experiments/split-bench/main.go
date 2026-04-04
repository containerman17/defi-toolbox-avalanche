// Cross-benchmark: Greedy vs Optimized vs GreedyFine across 17 token pairs.
package main

import (
	"flag"
	"fmt"
	"math"
	"math/big"
	"os"
	"sort"
	"strings"

	pf "defi-toolbox/pathfinder"
	"defi-toolbox/pathfinder/splitter"
	"defi-toolbox/quoter"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

var tokens = []struct {
	Name     string
	Address  common.Address
	Decimals int
	Amount   string
}{
	{"WAVAX", common.HexToAddress("0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7"), 18, "50000"},
	{"USDC", common.HexToAddress("0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E"), 6, "1000000"},
	{"USDT", common.HexToAddress("0x9702230A8Ea53601f5cD2dc00fDBc13d4dF4A8c7"), 6, "1000000"},
	{"sAVAX", common.HexToAddress("0x2b2C81e08f1Af8835a78Bb2A90AE924ACE0eA4bE"), 18, "50000"},
	{"WETH.e", common.HexToAddress("0x49D5c2BdFfac6CE2BFdB6640F4F80f226bc10bAB"), 18, "300"},
}

type strategy struct {
	name string
	run  func(*splitter.Params, *uint256.Int) *splitter.Result
}

func main() {
	stateServer := flag.String("state-server", "ws://localhost:7449/live", "state server WebSocket URL")
	poolLimit := flag.Int("pool-limit", 2000, "max pools to load")
	maxHops := flag.Int("max-hops", 3, "max hops per route")
	chunks := flag.Int("chunks", 10, "base number of chunks")
	flag.Parse()

	ls, err := statedb.Connect(*stateServer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "connected, block=%d\n", ls.Block())

	q := quoter.NewQuoter(ls, *poolLimit, *maxHops)
	q.StartBlockLoop()

	ready := make(chan struct{})
	q.SetOnBlock(func(block, timestamp uint64) {
		select {
		case ready <- struct{}{}:
		default:
		}
	})
	fmt.Fprintf(os.Stderr, "waiting for block...\n")
	<-ready
	fmt.Fprintf(os.Stderr, "block %d ready\n", ls.Block())

	ls.RLock()
	defer ls.RUnlock()

	cfg := ls.EVMConfig()
	ch := *chunks

	strategies := []strategy{
		{"greedy", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.Greedy(p, a, ch) }},
		{"optimized", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.Optimized(p, a, ch) }},
		{"greedyfine", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.GreedyFine(p, a, ch) }},
		{"gfast40", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.GreedyFast(p, a, ch*4) }},
		{"gfast100", func(p *splitter.Params, a *uint256.Int) *splitter.Result { return splitter.GreedyFast(p, a, 100) }},
	}

	pcts := make([][]float64, len(strategies))
	times := make([][]float64, len(strategies))
	for i := range strategies {
		pcts[i] = []float64{}
		times[i] = []float64{}
	}
	nPairs := 0

	fmt.Printf("%-14s", "PAIR")
	for _, s := range strategies {
		fmt.Printf("  %12s", s.name)
	}
	for _, s := range strategies {
		fmt.Printf("  %6s", s.name[:3]+"ms")
	}
	fmt.Println()
	fmt.Println(strings.Repeat("─", 14+len(strategies)*14+len(strategies)*8))

	for i, tIn := range tokens {
		for j, tOut := range tokens {
			if i == j {
				continue
			}

			amount := parseDecimalAmount(tIn.Amount, tIn.Decimals)
			if amount == nil || amount.Sign() <= 0 {
				continue
			}
			fullAmount := new(uint256.Int)
			fullAmount.SetFromBig(amount)

			params := &splitter.Params{
				PM:         q.PM(),
				BasePM:     q.PM(),
				Adj:        q.Adj(),
				Pools:      q.Pools(),
				State:      q.StateWithOverrides(),
				EVMConfig:  cfg,
				RouterAddr: q.RouterAddr(),
				Sender:     q.Sender(),
				TokenIn:    tIn.Address,
				TokenOut:   tOut.Address,
				MaxHops:    q.MaxHops(),
			}

			single := pf.FindBestRoute(params.PM, params.Adj, params.Pools, params.State,
				cfg, params.RouterAddr, params.Sender, tIn.Address, tOut.Address, fullAmount, q.MaxHops())
			if single == nil {
				continue
			}
			singleF := u256ToFloat(single.AmountOut, tOut.Decimals)
			nPairs++

			pair := tIn.Name + "→" + tOut.Name
			fmt.Printf("%-14s", pair)

			for si, s := range strategies {
				result := s.run(params, fullAmount)
				pct := 0.0
				ms := 0.0
				if result != nil {
					outF := u256ToFloat(&result.Total, tOut.Decimals)
					pct = pctImprovement(singleF, outF)
					ms = float64(result.ElapsedUs) / 1000.0
				}
				pcts[si] = append(pcts[si], pct)
				times[si] = append(times[si], ms)
				fmt.Printf("  %+11.4f%%", pct)
			}
			for si := range strategies {
				fmt.Printf("  %5.0fms", times[si][len(times[si])-1])
			}
			fmt.Println()
		}
	}

	if nPairs == 0 {
		return
	}

	fmt.Println()
	fmt.Println(strings.Repeat("═", 14+len(strategies)*14+len(strategies)*8))
	fmt.Println()

	for _, label := range []string{"MEDIAN", "AVG", "MIN", "MAX"} {
		fmt.Printf("%-14s", label)
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
			fmt.Printf("  %+11.4f%%", v)
		}
		fmt.Println()
	}

	fmt.Println()
	fmt.Printf("%-14s", "AVG TIME")
	for si := range strategies {
		fmt.Printf("  %10.0fms", mean(times[si]))
	}
	fmt.Println()
	fmt.Printf("%-14s", "MED TIME")
	for si := range strategies {
		sorted := make([]float64, len(times[si]))
		copy(sorted, times[si])
		sort.Float64s(sorted)
		fmt.Printf("  %10.0fms", median(sorted))
	}
	fmt.Println()

	// Head-to-head
	fmt.Println()
	fmt.Println("HEAD-TO-HEAD vs greedy:")
	for si := 1; si < len(strategies); si++ {
		wins, losses, t := 0, 0, 0
		for pi := 0; pi < nPairs; pi++ {
			if pcts[si][pi] > pcts[0][pi]+0.0001 {
				wins++
			} else if pcts[si][pi] < pcts[0][pi]-0.0001 {
				losses++
			} else {
				t++
			}
		}
		fmt.Printf("  %-12s  W=%d  L=%d  T=%d\n", strategies[si].name, wins, losses, t)
	}
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
