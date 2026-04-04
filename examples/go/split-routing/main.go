// Split routing example: compares single-path vs N-way split quoting.
//
// Connects to a state server, waits for first block, then:
// 1. Quotes a swap at full volume (single best path)
// 2. Splits the same volume into N chunks, re-running BFS after each chunk
//    with an overlay that reflects depleted pool state from previous chunks
// 3. Prints per-leg details and total comparison
//
// Supports both A→B and circular (A→A) routes.
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

	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
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
	chunks := flag.Int("chunks", 10, "number of split chunks")
	amountStr := flag.String("amount", "50000", "amount in whole tokens")
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

	// Connect first — need state to query decimals
	ls, err := statedb.Connect(*stateServer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "connected, block=%d\n", ls.Block())

	// Query decimals from contracts
	decimalsIn := queryDecimals(ls.State(), tokenIn)
	decimalsOut := queryDecimals(ls.State(), tokenOut)
	fmt.Fprintf(os.Stderr, "tokenIn decimals=%d, tokenOut decimals=%d\n", decimalsIn, decimalsOut)

	// Parse amount (supports decimals like "0.1")
	fullAmountBig := parseDecimalAmount(*amountStr, decimalsIn)
	if fullAmountBig == nil || fullAmountBig.Sign() <= 0 {
		fmt.Fprintf(os.Stderr, "invalid amount: %s\n", *amountStr)
		os.Exit(1)
	}
	fullAmount := new(uint256.Int)
	fullAmount.SetFromBig(fullAmountBig)

	chunkAmountBig := new(big.Int).Div(fullAmountBig, big.NewInt(int64(*chunks)))
	chunkAmount := new(uint256.Int)
	chunkAmount.SetFromBig(chunkAmountBig)

	// Quoter setup
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

	// Grab references
	pm := q.PM()
	adj := q.Adj()
	pools := q.Pools()
	baseState := q.StateWithOverrides()
	routerAddr := q.RouterAddr()
	sender := q.Sender()
	mh := q.MaxHops()

	ls.RLock()
	defer ls.RUnlock()

	cfg := ls.EVMConfig()
	evmCtx := statedb.GetCachedContext(cfg)

	// ── Single path baseline ──────────────────────────────────────────

	fmtOut := func(amount *uint256.Int) string { return formatTokenAmount(amount, decimalsOut) }
	fmtIn := func(amount *uint256.Int) string { return formatTokenAmount(amount, decimalsIn) }

	cyclic := tokenIn == tokenOut
	pairLabel := fmt.Sprintf("%s → %s", tokenIn.Hex()[:8], tokenOut.Hex()[:8])
	if cyclic {
		pairLabel = fmt.Sprintf("%s → %s (cyclic)", tokenIn.Hex()[:8], tokenOut.Hex()[:8])
	}

	fmt.Printf("\n=== SINGLE PATH: %s %s ===\n", *amountStr, pairLabel)
	t0 := time.Now()
	singleRoute := pf.FindBestRoute(pm, adj, pools, baseState, cfg, routerAddr, sender,
		tokenIn, tokenOut, fullAmount, mh)
	singleMs := time.Since(t0).Milliseconds()

	if singleRoute == nil {
		fmt.Fprintf(os.Stderr, "no single route found\n")
		os.Exit(1)
	}

	singleOut := singleRoute.AmountOut
	if cyclic {
		fmt.Printf("  output:  %s (%s raw)  loss: %s\n", fmtIn(singleOut), singleOut.Dec(),
			fmtIn(new(uint256.Int).Sub(fullAmount, singleOut)))
	} else {
		fmt.Printf("  output:  %s (%s raw)\n", fmtOut(singleOut), singleOut.Dec())
	}
	fmt.Printf("  gas:     %d\n", singleRoute.GasUsed)
	fmt.Printf("  path:    %s\n", formatPath(singleRoute, pools))
	fmt.Printf("  time:    %dms\n", singleMs)
	fmt.Printf("  stats:   %d formula + %d evm = %d quotes\n",
		singleRoute.Stats.FormulaQuotes, singleRoute.Stats.EVMQuotes, singleRoute.Stats.TotalQuotes)

	// ── Split routing ─────────────────────────────────────────────────

	fmt.Printf("\n=== SPLIT ROUTING: %d chunks of %s ===\n", *chunks, fmtIn(chunkAmount))

	accDirtySlots := make(map[common.Address]map[common.Hash]common.Hash)
	var totalOut uint256.Int
	var totalGas uint64
	var totalFormulaQuotes, totalEVMQuotes int
	t0 = time.Now()

	currentState := baseState

	var totalOverlayUs, totalBfsUs, totalEvmUs, totalMergeUs, totalStateUs int64

	for i := 0; i < *chunks; i++ {
		var pqs formulas.PoolQuoterSource
		var affectedCount int
		tPhase := time.Now()
		if i == 0 {
			pqs = pm
		} else {
			overlay := formulas.NewPoolManagerOverlay(pm, accDirtySlots)
			affectedCount = overlay.AffectedCount()
			pqs = overlay
		}
		totalOverlayUs += time.Since(tPhase).Microseconds()

		tPhase = time.Now()
		route := pf.FindBestRoute(pqs, adj, pools, currentState, cfg, routerAddr, sender,
			tokenIn, tokenOut, chunkAmount, mh)
		totalBfsUs += time.Since(tPhase).Microseconds()

		if route == nil {
			fmt.Printf("  leg %d: no route found (liquidity exhausted?)\n", i+1)
			break
		}

		totalOut.Add(&totalOut, route.AmountOut)
		totalGas += route.GasUsed
		totalFormulaQuotes += route.Stats.FormulaQuotes
		totalEVMQuotes += route.Stats.EVMQuotes

		fmt.Printf("  leg %2d: %s  gas=%d  affected=%d  path=%s\n",
			i+1, fmtOut(route.AmountOut), route.GasUsed, affectedCount, formatPath(route, pools))

		// EVM-execute this leg on a CallState to capture dirty slots
		tPhase = time.Now()
		cs := statedb.NewCallState(currentState)
		ret, _, execErr := evmCtx.ExecuteWithCallState(cs, sender, routerAddr, route.Calldata)
		totalEvmUs += time.Since(tPhase).Microseconds()
		if execErr != nil || len(ret) < 32 {
			fmt.Printf("         EVM execution failed: %v\n", execErr)
			break
		}

		// Merge dirty slots from this execution
		tPhase = time.Now()
		for addr, slots := range cs.StorageOverrides() {
			if accDirtySlots[addr] == nil {
				accDirtySlots[addr] = make(map[common.Hash]common.Hash)
			}
			for slot, val := range slots {
				accDirtySlots[addr][slot] = val
			}
		}
		totalMergeUs += time.Since(tPhase).Microseconds()

		// Build new state overlay with accumulated dirty slots for next EVM verification
		tPhase = time.Now()
		currentState = baseState.NewOverlay()
		for addr, slots := range accDirtySlots {
			for slot, val := range slots {
				currentState.SetStorageSlot(addr, slot, val)
			}
		}
		totalStateUs += time.Since(tPhase).Microseconds()
	}

	splitMs := time.Since(t0).Milliseconds()

	dirtySlotCount := 0
	for _, slots := range accDirtySlots {
		dirtySlotCount += len(slots)
	}

	// ── Comparison ────────────────────────────────────────────────────

	fmt.Printf("\n=== COMPARISON ===\n")
	if cyclic {
		// For circular routes, show output and loss (input - output)
		splitInput := new(uint256.Int).Mul(chunkAmount, uint256.NewInt(uint64(*chunks)))
		singleLoss := new(uint256.Int).Sub(fullAmount, singleOut)
		splitLoss := new(uint256.Int)
		if splitInput.Gt(&totalOut) {
			splitLoss.Sub(splitInput, &totalOut)
		}
		fmt.Printf("  single path:  %s out  loss %s  gas=%d\n",
			fmtIn(singleOut), fmtIn(singleLoss), singleRoute.GasUsed)
		fmt.Printf("  split (%dx):  %s out  loss %s  gas=%d\n",
			*chunks, fmtIn(&totalOut), fmtIn(splitLoss), totalGas)

		if totalOut.Gt(singleOut) {
			diff := new(uint256.Int).Sub(&totalOut, singleOut)
			fmt.Printf("  improvement:  +%s (less loss)\n", fmtIn(diff))
		} else if singleOut.Gt(&totalOut) {
			diff := new(uint256.Int).Sub(singleOut, &totalOut)
			fmt.Printf("  worse by:     -%s\n", fmtIn(diff))
		} else {
			fmt.Printf("  improvement:  none (identical)\n")
		}
	} else {
		fmt.Printf("  single path:  %s  gas=%d\n", fmtOut(singleOut), singleRoute.GasUsed)
		fmt.Printf("  split (%dx):  %s  gas=%d\n", *chunks, fmtOut(&totalOut), totalGas)

		if totalOut.Gt(singleOut) {
			diff := new(uint256.Int).Sub(&totalOut, singleOut)
			fmt.Printf("  improvement:  +%s\n", fmtOut(diff))
		} else if singleOut.Gt(&totalOut) {
			diff := new(uint256.Int).Sub(singleOut, &totalOut)
			fmt.Printf("  worse by:     -%s\n", fmtOut(diff))
		} else {
			fmt.Printf("  improvement:  none (identical)\n")
		}
	}

	fmt.Printf("  extra gas:    %d\n", int64(totalGas)-int64(singleRoute.GasUsed))
	fmt.Printf("  dirty slots:  %d\n", dirtySlotCount)
	fmt.Printf("  time:         %dms single, %dms split\n", singleMs, splitMs)
	fmt.Printf("  quotes:       %d formula + %d evm = %d total\n",
		totalFormulaQuotes, totalEVMQuotes, totalFormulaQuotes+totalEVMQuotes)
	fmt.Printf("\n=== PROFILE (split) ===\n")
	fmt.Printf("  overlay:  %5dμs  (create PoolManagerOverlay, scan depSlots)\n", totalOverlayUs)
	fmt.Printf("  bfs:      %5dμs  (FindBestRoute: formula BFS + EVM verify)\n", totalBfsUs)
	fmt.Printf("  evm:      %5dμs  (EVM execute leg for dirty slots)\n", totalEvmUs)
	fmt.Printf("  merge:    %5dμs  (merge dirty slots)\n", totalMergeUs)
	fmt.Printf("  state:    %5dμs  (build StateDB overlay)\n", totalStateUs)
	fmt.Printf("  total:    %5dμs\n", totalOverlayUs+totalBfsUs+totalEvmUs+totalMergeUs+totalStateUs)
}

// ── Formatting helpers ──────────────────────────────────────────────

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

func formatPath(route *pf.Route, pools []pf.Pool) string {
	if route == nil || len(route.Steps) == 0 {
		return "(none)"
	}
	dexMap := make(map[common.Address]string, len(pools))
	for _, p := range pools {
		dexMap[p.Address] = p.Dex
	}

	var parts []string
	for _, s := range route.Steps {
		dex := dexMap[s.Pool]
		if dex == "" {
			dex = s.Pool.Hex()[:10]
		}
		parts = append(parts, dex)
	}
	return strings.Join(parts, " → ")
}

// parseDecimalAmount parses a decimal string like "0.1" or "50000" into wei.
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

// queryDecimals reads the decimals() value for an ERC-20 token from state.
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
