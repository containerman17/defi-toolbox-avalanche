// Split routing comparison: single path vs greedy split vs optimized split.
//
// Connects to a state server, waits for first block, then runs three
// strategies on the same swap and prints a comparison table.
package main

import (
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"os/signal"
	"runtime/pprof"
	"syscall"

	pf "defi-toolbox/pathfinder"
	"defi-toolbox/pathfinder/splitter"
	"defi-toolbox/quoter"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

var (
	WAVAX = common.HexToAddress("0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7")
	USDT  = common.HexToAddress("0x9702230A8Ea53601f5cD2dc00fDBc13d4dF4A8c7")
)

func main() {
	stateServer := flag.String("state-server", "ws://localhost:7449/live", "state server WebSocket URL")
	poolLimit := flag.Int("pool-limit", 2000, "max pools to load")
	maxHops := flag.Int("max-hops", 3, "max hops per route")
	chunks := flag.Int("chunks", 10, "number of chunks for greedy split")
	amountStr := flag.String("amount", "50000", "amount in whole or decimal tokens")
	tokenInStr := flag.String("token-in", WAVAX.Hex(), "input token address")
	tokenOutStr := flag.String("token-out", USDT.Hex(), "output token address")
	cpuprofile := flag.String("cpuprofile", "", "write cpu profile to file")
	flag.Parse()

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cpuprofile: %v\n", err)
			os.Exit(1)
		}
		pprof.StartCPUProfile(f)
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			pprof.StopCPUProfile()
			f.Close()
			os.Exit(0)
		}()
		defer func() { pprof.StopCPUProfile(); f.Close() }()
	}

	tokenIn := common.HexToAddress(*tokenInStr)
	tokenOut := common.HexToAddress(*tokenOutStr)

	ls, err := statedb.Connect(*stateServer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "connected, block=%d\n", ls.Block())

	decimalsIn := queryDecimals(ls.State(), tokenIn)
	decimalsOut := queryDecimals(ls.State(), tokenOut)
	fmt.Fprintf(os.Stderr, "tokenIn decimals=%d, tokenOut decimals=%d\n", decimalsIn, decimalsOut)

	fullAmountBig := parseDecimalAmount(*amountStr, decimalsIn)
	if fullAmountBig == nil || fullAmountBig.Sign() <= 0 {
		fmt.Fprintf(os.Stderr, "invalid amount: %s\n", *amountStr)
		os.Exit(1)
	}
	fullAmount := new(uint256.Int)
	fullAmount.SetFromBig(fullAmountBig)

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
	params := &splitter.Params{
		PM:         q.PM(),
		BasePM:     q.PM(),
		Adj:        q.Adj(),
		Pools:      q.Pools(),
		State:      q.StateWithOverrides(),
		EVMConfig:  cfg,
		RouterAddr: q.RouterAddr(),
		Sender:     q.Sender(),
		TokenIn:    tokenIn,
		TokenOut:   tokenOut,
		MaxHops:    q.MaxHops(),
	}

	fmtOut := func(amount *uint256.Int) string { return formatTokenAmount(amount, decimalsOut) }

	cyclic := tokenIn == tokenOut
	pairLabel := fmt.Sprintf("%s → %s", tokenIn.Hex()[:8], tokenOut.Hex()[:8])
	if cyclic {
		pairLabel += " (cyclic)"
	}
	fmt.Printf("\n=== %s %s ===\n", *amountStr, pairLabel)

	// ── 1. Single path ───────────────────────────────────────────────

	fmt.Printf("\n--- Single Path ---\n")
	t0 := time.Now()
	singleRoute := pf.FindBestRoute(params.PM, params.Adj, params.Pools, params.State,
		cfg, params.RouterAddr, params.Sender, tokenIn, tokenOut, fullAmount, params.MaxHops)
	singleMs := time.Since(t0).Milliseconds()

	if singleRoute == nil {
		fmt.Fprintf(os.Stderr, "no single route found\n")
		os.Exit(1)
	}
	fmt.Printf("  output:  %s\n", fmtOut(singleRoute.AmountOut))
	fmt.Printf("  gas:     %d\n", singleRoute.GasUsed)
	fmt.Printf("  time:    %dms\n", singleMs)
	fmt.Printf("  path:    %s\n", formatPath(singleRoute.Steps, params.Pools))

	// ── 2. Greedy split ──────────────────────────────────────────────

	fmt.Printf("\n--- Greedy Split (%d chunks) ---\n", *chunks)
	greedy := splitter.Greedy(params, fullAmount, *chunks)
	if greedy != nil {
		for i, leg := range greedy.Legs {
			fmt.Printf("  leg %2d: %s  gas=%d  path=%s\n",
				i+1, fmtOut(&leg.Output), leg.GasUsed, formatPath(leg.Steps, params.Pools))
		}
		fmt.Printf("  total:   %s  gas=%d  time=%dμs\n",
			fmtOut(&greedy.Total), greedy.TotalGas, greedy.ElapsedUs)
	} else {
		fmt.Printf("  (no result)\n")
	}

	// ── 3. Optimized split ───────────────────────────────────────────

	fmt.Printf("\n--- Optimized Split ---\n")
	optimized := splitter.Optimized(params, fullAmount, *chunks)
	if optimized != nil {
		for i, leg := range optimized.Legs {
			fmt.Printf("  leg %2d: %s  vol=%s  gas=%d  path=%s\n",
				i+1, fmtOut(&leg.Output), formatTokenAmount(&leg.Volume, decimalsIn),
				leg.GasUsed, formatPath(leg.Steps, params.Pools))
		}
		fmt.Printf("  total:   %s  gas=%d  time=%dμs\n",
			fmtOut(&optimized.Total), optimized.TotalGas, optimized.ElapsedUs)
	} else {
		fmt.Printf("  (no result)\n")
	}

	// ── 3b. GreedyFine split ────────────────────────────────────────

	fmt.Printf("\n--- GreedyFine Split (%d chunks) ---\n", *chunks*4)
	greedyfine := splitter.GreedyFine(params, fullAmount, *chunks)
	if greedyfine != nil {
		fmt.Printf("  total:   %s  gas=%d  time=%dμs  legs=%d\n",
			fmtOut(&greedyfine.Total), greedyfine.TotalGas, greedyfine.ElapsedUs, len(greedyfine.Legs))
	} else {
		fmt.Printf("  (no result)\n")
	}

	// ── 4. Squished execution ────────────────────────────────────────
	// Take the optimized legs and execute them as a single swap() call
	// instead of separate calls, then compare gas.

	var squishedGas uint64
	var squishedOut uint256.Int
	var squishedV2Gas uint64
	var squishedV2Out uint256.Int
	if optimized != nil && len(optimized.Legs) > 1 {
		squishedRoutes := make([]pf.SquishRoute, len(optimized.Legs))
		naiveSteps := 0
		for i, leg := range optimized.Legs {
			squishedRoutes[i] = pf.SquishRoute{
				Steps:  leg.Steps,
				Volume: new(uint256.Int).Set(&leg.Volume),
			}
			naiveSteps += len(leg.Steps)
		}

		// ── 4a. Suffix-only merge ────────────────────────────────────
		fmt.Printf("\n--- Squished v1 (suffix merge) ---\n")
		mergedSteps, _ := pf.MergeRoutes(squishedRoutes)
		fmt.Printf("  legs: %d  naive steps: %d  merged steps: %d  (%.0f%% reduction)\n",
			len(optimized.Legs), naiveSteps, len(mergedSteps),
			float64(naiveSteps-len(mergedSteps))/float64(naiveSteps)*100)

		calldata := pf.EncodeSquished(squishedRoutes, uint256.NewInt(0))
		evmCtx := statedb.GetCachedContext(cfg)
		cs := statedb.NewCallState(params.State)
		ret, gasUsed, err := evmCtx.ExecuteWithCallState(cs, params.Sender, params.RouterAddr, calldata)
		if err != nil || len(ret) < 32 {
			fmt.Printf("  EVM error: %v\n", err)
		} else {
			squishedOut.SetBytes(ret[:32])
			squishedGas = gasUsed
			if squishedOut.Bytes32()[0]&0x80 != 0 {
				fmt.Printf("  negative output (reverted)\n")
			} else {
				fmt.Printf("  output:  %s  gas=%d  steps=%d\n", fmtOut(&squishedOut), squishedGas, len(mergedSteps))
			}
		}

		// ── 4b. Full merge (suffix + first-hop) ──────────────────────
		fmt.Printf("\n--- Squished v2 (suffix + first-hop merge) ---\n")
		quoterFn := func(step pf.RouteStep, amountIn *uint256.Int) uint256.Int {
			return pf.QuotePath(params.PM, []pf.RouteStep{step}, amountIn)
		}
		mergedStepsV2, mergedAmountsV2 := pf.MergeRoutesWithQuoter(squishedRoutes, quoterFn)
		fmt.Printf("  legs: %d  naive steps: %d  merged steps: %d  (%.0f%% reduction)\n",
			len(optimized.Legs), naiveSteps, len(mergedStepsV2),
			float64(naiveSteps-len(mergedStepsV2))/float64(naiveSteps)*100)
		for i, s := range mergedStepsV2 {
			dex := s.Pool.Hex()[:10]
			for _, p := range params.Pools {
				if p.Address == s.Pool {
					dex = p.Dex
					break
				}
			}
			amt := "balance"
			if !mergedAmountsV2[i].IsZero() {
				amt = formatTokenAmount(mergedAmountsV2[i], decimalsIn)
			}
			fmt.Printf("    step %d: %s(%s→%s)  amt=%s\n",
				i+1, dex, s.TokenIn.Hex()[:8], s.TokenOut.Hex()[:8], amt)
		}

		calldataV2 := pf.EncodeSquishedWithQuoter(squishedRoutes, uint256.NewInt(0), quoterFn)
		csV2 := statedb.NewCallState(params.State)
		retV2, gasUsedV2, errV2 := evmCtx.ExecuteWithCallState(csV2, params.Sender, params.RouterAddr, calldataV2)
		if errV2 != nil || len(retV2) < 32 {
			fmt.Printf("  EVM error: %v\n", errV2)
		} else {
			squishedV2Out.SetBytes(retV2[:32])
			squishedV2Gas = gasUsedV2
			if squishedV2Out.Bytes32()[0]&0x80 != 0 {
				fmt.Printf("  negative output (reverted)\n")
			} else {
				fmt.Printf("  output:  %s  gas=%d  steps=%d\n", fmtOut(&squishedV2Out), squishedV2Gas, len(mergedStepsV2))
				if squishedGas > 0 {
					fmt.Printf("  v1→v2: %d → %d gas  (saved %d more, %.1f%%)\n",
						squishedGas, squishedV2Gas,
						squishedGas-squishedV2Gas,
						float64(squishedGas-squishedV2Gas)/float64(squishedGas)*100)
				}
			}
		}
	}

	// ── Comparison ───────────────────────────────────────────────────

	fmt.Printf("\n=== COMPARISON ===\n")
	fmt.Printf("  single:     %s  gas=%-8d  time=%dms\n",
		fmtOut(singleRoute.AmountOut), singleRoute.GasUsed, singleMs)
	if greedy != nil {
		diff := diffStr(&greedy.Total, singleRoute.AmountOut, decimalsOut)
		fmt.Printf("  greedy:     %s  gas=%-8d  time=%dμs  %s\n",
			fmtOut(&greedy.Total), greedy.TotalGas, greedy.ElapsedUs, diff)
	}
	if optimized != nil {
		diff := diffStr(&optimized.Total, singleRoute.AmountOut, decimalsOut)
		fmt.Printf("  optimized:  %s  gas=%-8d  time=%dμs  %s\n",
			fmtOut(&optimized.Total), optimized.TotalGas, optimized.ElapsedUs, diff)
	}
	if greedyfine != nil {
		diff := diffStr(&greedyfine.Total, singleRoute.AmountOut, decimalsOut)
		fmt.Printf("  greedyfine: %s  gas=%-8d  time=%dμs  %s\n",
			fmtOut(&greedyfine.Total), greedyfine.TotalGas, greedyfine.ElapsedUs, diff)
	}
	if squishedGas > 0 {
		diff := diffStr(&squishedOut, singleRoute.AmountOut, decimalsOut)
		fmt.Printf("  squished v1:%s  gas=%-8d  %s\n",
			fmtOut(&squishedOut), squishedGas, diff)
	}
	if squishedV2Gas > 0 {
		diff := diffStr(&squishedV2Out, singleRoute.AmountOut, decimalsOut)
		fmt.Printf("  squished v2:%s  gas=%-8d  %s\n",
			fmtOut(&squishedV2Out), squishedV2Gas, diff)
	}
}

// ── Helpers ─────────────────────────────────────────────────────────

func diffStr(total, baseline *uint256.Int, decimals int) string {
	if total.Gt(baseline) {
		d := new(uint256.Int).Sub(total, baseline)
		return fmt.Sprintf("+%s", formatTokenAmount(d, decimals))
	} else if baseline.Gt(total) {
		d := new(uint256.Int).Sub(baseline, total)
		return fmt.Sprintf("-%s", formatTokenAmount(d, decimals))
	}
	return "same"
}

func formatTokenAmount(amount *uint256.Int, decimals int) string {
	d := amount.Dec()
	if len(d) <= decimals {
		return "0." + strings.Repeat("0", decimals-len(d)) + d
	}
	whole := d[:len(d)-decimals]
	frac := d[len(d)-decimals:]
	return addCommas(whole) + "." + frac
}

func addCommas(s string) string {
	if len(s) <= 3 {
		return s
	}
	return addCommas(s[:len(s)-3]) + "," + s[len(s)-3:]
}

func formatPath(steps []pf.RouteStep, pools []pf.Pool) string {
	if len(steps) == 0 {
		return "(none)"
	}
	dexMap := make(map[common.Address]string, len(pools))
	for _, p := range pools {
		dexMap[p.Address] = p.Dex
	}
	var parts []string
	for _, s := range steps {
		dex := dexMap[s.Pool]
		if dex == "" {
			dex = s.Pool.Hex()[:10]
		}
		parts = append(parts, dex)
	}
	return strings.Join(parts, " → ")
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

func queryDecimals(state *statedb.StateDB, token common.Address) int {
	calldata := []byte{0x31, 0x3c, 0xe5, 0x67}
	cfg := statedb.EVMConfig{BlockNumber: 1, Timestamp: 1, ChainID: 43114}
	ret, _, err := statedb.ExecuteCall(state, cfg, common.Address{}, token, calldata)
	if err != nil || len(ret) < 32 {
		return 18
	}
	var dec uint256.Int
	dec.SetBytes(ret[:32])
	return int(dec.Uint64())
}
