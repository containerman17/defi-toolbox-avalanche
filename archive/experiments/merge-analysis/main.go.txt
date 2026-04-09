// Merge analysis: runs 5x5 token pairs in both directions at ~$10k volume
// with 20-chunk optimized splits, then reports duplicate pool calls in the
// merged output. Temporary script for finding merge optimization opportunities.
package main

import (
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"

	pf "defi-toolbox/pathfinder"
	"defi-toolbox/pathfinder/splitter"
	"defi-toolbox/quoter"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// Tokens with ~$10k amounts in whole units (pre-decimal).
var tokens = []struct {
	Name     string
	Address  common.Address
	Decimals int
	Amount   string // ~$10k worth
}{
	{"WAVAX", common.HexToAddress("0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7"), 18, "500"},
	{"USDC", common.HexToAddress("0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E"), 6, "10000"},
	{"USDT", common.HexToAddress("0x9702230A8Ea53601f5cD2dc00fDBc13d4dF4A8c7"), 6, "10000"},
	{"sAVAX", common.HexToAddress("0x2b2C81e08f1Af8835a78Bb2A90AE924ACE0eA4bE"), 18, "500"},
	{"WETH.e", common.HexToAddress("0x49D5c2BdFfac6CE2BFdB6640F4F80f226bc10bAB"), 18, "3"},
}

func main() {
	stateServer := flag.String("state-server", "ws://localhost:7449/live", "state server WebSocket URL")
	poolLimit := flag.Int("pool-limit", 2000, "max pools to load")
	maxHops := flag.Int("max-hops", 3, "max hops per route")
	chunks := flag.Int("chunks", 20, "number of chunks")
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

	type result struct {
		From, To     string
		NaiveSteps   int
		V1Steps      int
		V2Steps      int
		Duplicates   []string // pool+direction keys that appear >1 time in v2
		GasSeparate  uint64
		GasV1        uint64
		GasV2        uint64
	}

	var results []result

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

			optimized := splitter.Optimized(params, fullAmount, *chunks)
			if optimized == nil || len(optimized.Legs) < 2 {
				continue
			}

			routes := make([]pf.SquishRoute, len(optimized.Legs))
			naiveSteps := 0
			for k, leg := range optimized.Legs {
				routes[k] = pf.SquishRoute{
					Steps:  leg.Steps,
					Volume: new(uint256.Int).Set(&leg.Volume),
				}
				naiveSteps += len(leg.Steps)
			}

			// V1: suffix only
			v1Steps, _ := pf.MergeRoutes(routes)

			// V2: suffix + first-hop + collapse
			quoterFn := func(step pf.RouteStep, amountIn *uint256.Int) uint256.Int {
				return pf.QuotePath(params.PM, []pf.RouteStep{step}, amountIn)
			}
			v2Steps, v2Amounts := pf.MergeRoutesWithQuoter(routes, quoterFn)

			// Find duplicates in v2
			keyCounts := make(map[string]int)
			keyLabels := make(map[string]string)
			for k, s := range v2Steps {
				key := s.Pool.Hex()[:10] + "(" + s.TokenIn.Hex()[:8] + "→" + s.TokenOut.Hex()[:8] + ")"
				// Find DEX name
				for _, p := range params.Pools {
					if p.Address == s.Pool {
						key = p.Dex + "(" + s.TokenIn.Hex()[:8] + "→" + s.TokenOut.Hex()[:8] + ")"
						break
					}
				}
				mk := s.Pool.Hex() + s.TokenIn.Hex() + s.TokenOut.Hex()
				keyCounts[mk]++
				keyLabels[mk] = key
				_ = v2Amounts[k]
			}
			var dups []string
			for mk, count := range keyCounts {
				if count > 1 {
					dups = append(dups, fmt.Sprintf("%s x%d", keyLabels[mk], count))
				}
			}

			// EVM verify v1 and v2
			evmCtx := statedb.GetCachedContext(cfg)

			calldataV1 := pf.EncodeSquished(routes, uint256.NewInt(0))
			csV1 := statedb.NewCallState(params.State)
			_, gasV1, errV1 := evmCtx.ExecuteWithCallState(csV1, params.Sender, params.RouterAddr, calldataV1)
			if errV1 != nil {
				gasV1 = 0
			}

			calldataV2 := pf.EncodeSquishedWithQuoter(routes, uint256.NewInt(0), quoterFn)
			csV2 := statedb.NewCallState(params.State)
			_, gasV2, errV2 := evmCtx.ExecuteWithCallState(csV2, params.Sender, params.RouterAddr, calldataV2)
			if errV2 != nil {
				gasV2 = 0
			}

			r := result{
				From:        tIn.Name,
				To:          tOut.Name,
				NaiveSteps:  naiveSteps,
				V1Steps:     len(v1Steps),
				V2Steps:     len(v2Steps),
				Duplicates:  dups,
				GasSeparate: optimized.TotalGas,
				GasV1:       gasV1,
				GasV2:       gasV2,
			}
			results = append(results, r)

			dupStr := ""
			if len(dups) > 0 {
				dupStr = "  DUPS: " + strings.Join(dups, ", ")
			}
			gasStr := ""
			if gasV1 > 0 && gasV2 > 0 && gasV1 != gasV2 {
				saved := int64(gasV1) - int64(gasV2)
				gasStr = fmt.Sprintf("  gas: %dk→%dk (Δ%dk)", gasV1/1000, gasV2/1000, saved/1000)
			}
			fmt.Printf("%-8s → %-8s  naive=%2d  v1=%2d  v2=%2d%s%s\n",
				tIn.Name, tOut.Name, naiveSteps, len(v1Steps), len(v2Steps), gasStr, dupStr)

			// Print full step details when duplicates found
			if len(dups) > 0 {
				for k, s := range v2Steps {
					dex := s.Pool.Hex()[:10]
					for _, p := range params.Pools {
						if p.Address == s.Pool {
							dex = p.Dex
							break
						}
					}
					amtStr := "balance"
					if !v2Amounts[k].IsZero() {
						amtStr = v2Amounts[k].Dec()
					}
					fmt.Printf("  [%d] %s pool=%s (%s→%s) amt=%s\n",
						k, dex, s.Pool.Hex()[:10], s.TokenIn.Hex()[:10], s.TokenOut.Hex()[:10], amtStr)
				}
			}
		}
	}

	// Summary
	fmt.Printf("\n=== DUPLICATES FOUND ===\n")
	anyDups := false
	for _, r := range results {
		if len(r.Duplicates) > 0 {
			anyDups = true
			fmt.Printf("%-8s → %-8s: %s\n", r.From, r.To, strings.Join(r.Duplicates, ", "))
		}
	}
	if !anyDups {
		fmt.Printf("(none)\n")
	}
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
