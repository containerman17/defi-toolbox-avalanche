package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	router "defi-toolbox/contracts"
	"defi-toolbox/formulas"
	pf "defi-toolbox/pathfinder"
	"defi-toolbox/statedb"
	poolcollector "defi-toolbox/tools/pool-collector"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// Hub tokens for cyclic arbitrage.
var (
	WAVAX = common.HexToAddress("0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7")
	USDC  = common.HexToAddress("0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E")
	USDT  = common.HexToAddress("0x9702230A8Ea53601f5cD2dc00fDBc13d4dF4A8c7")
	WETHe = common.HexToAddress("0x49D5c2BdFfac6CE2BFdB6640F4F80f226bc10bAB")

	hubs = []common.Address{WAVAX, USDC, USDT, WETHe}
)

// Probe amounts per hub token (4 orders of magnitude each).
var probeAmounts = map[common.Address][]*uint256.Int{
	WAVAX: {
		toWei(1, 17),  // 0.1 AVAX
		toWei(1, 18),  // 1 AVAX
		toWei(10, 18), // 10 AVAX
		toWei(100, 18), // 100 AVAX
	},
	USDC: {
		toWei(2, 6),    // 2 USDC
		toWei(20, 6),   // 20 USDC
		toWei(200, 6),  // 200 USDC
		toWei(2000, 6), // 2000 USDC
	},
	USDT: {
		toWei(2, 6),    // 2 USDT
		toWei(20, 6),   // 20 USDT
		toWei(200, 6),  // 200 USDT
		toWei(2000, 6), // 2000 USDT
	},
	WETHe: {
		toWei(1, 14),  // 0.0001 ETH
		toWei(1, 15),  // 0.001 ETH
		toWei(1, 16),  // 0.01 ETH
		toWei(1, 17),  // 0.1 ETH
	},
}

func toWei(mantissa, decimals uint64) *uint256.Int {
	base := new(uint256.Int).Exp(uint256.NewInt(10), uint256.NewInt(decimals))
	return new(uint256.Int).Mul(uint256.NewInt(mantissa), base)
}

func hubName(hub common.Address) string {
	switch hub {
	case WAVAX:
		return "WAVAX"
	case USDC:
		return "USDC"
	case USDT:
		return "USDT"
	case WETHe:
		return "WETH.e"
	}
	return hub.Hex()[:10]
}

func main() {
	rpcURL := "http://localhost:9650/ext/bc/C/rpc"
	maxHops := 4
	poolLimit := 2000
	dryRun := true

	for i, arg := range os.Args {
		switch arg {
		case "--rpc":
			if i+1 < len(os.Args) {
				rpcURL = os.Args[i+1]
			}
		case "--max-hops":
			if i+1 < len(os.Args) {
				fmt.Sscanf(os.Args[i+1], "%d", &maxHops)
			}
		case "--pool-limit":
			if i+1 < len(os.Args) {
				fmt.Sscanf(os.Args[i+1], "%d", &poolLimit)
			}
		case "--execute":
			dryRun = false
		}
	}
	if maxHops < 2 {
		maxHops = 2
	}
	if maxHops > 4 {
		maxHops = 4
	}

	fmt.Fprintf(os.Stderr, "[arb4] two-phase BFS arbitrage starting...\n")
	fmt.Fprintf(os.Stderr, "[arb4] max hops: %d, pool limit: %d\n", maxHops, poolLimit)
	if dryRun {
		fmt.Fprintf(os.Stderr, "[arb4] mode: DRY RUN (pass --execute --rpc <url> and set ARB_PRIVATE_KEY to go live)\n")
	} else {
		fmt.Fprintf(os.Stderr, "[arb4] mode: LIVE EXECUTION\n")
	}

	// Load formula registry + pools
	registry := formulas.LoadEmbeddedRegistry()
	pools := poolcollector.EmbeddedPools(poolLimit)
	fmt.Fprintf(os.Stderr, "[arb4] loaded %d pools\n", len(pools))

	// Connect to state server
	ls, err := statedb.Connect("ws://localhost:7449/live")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[arb4] failed to connect: %v\n", err)
		os.Exit(1)
	}
	state := ls.State()

	// Create PoolManager
	stateReader := func(addr common.Address, slot common.Hash) common.Hash {
		return state.GetState(addr, slot)
	}
	pm := formulas.NewPoolManager(registry, stateReader)
	for i := range pools {
		p := &pools[i]
		if len(p.Tokens) >= 2 {
			pm.SetPoolTokens(p.Address, p.Tokens...)
		}
		pm.SetPoolType(p.Address, p.PoolType, p.Dex)
	}
	pm.SetBlockTimestamp(ls.Timestamp())

	// Build adjacency graph
	adj := pf.BuildAdjacency(pools, registry)
	routerAddr := router.DeployedRouter
	sender := pf.DUMMY_SENDER

	// Build persistent state overlay with sender overrides
	senderOverrides := router.BuildSenderOverrides(sender, routerAddr, pools)
	stateWithOverrides := pf.ApplyOverridesFlat(state, senderOverrides)

	// Set up executor if live mode
	var executor *Executor
	if !dryRun {
		privKey := os.Getenv("ARB_PRIVATE_KEY")
		if privKey == "" {
			fmt.Fprintf(os.Stderr, "[arb4] ERROR: ARB_PRIVATE_KEY env var required for --execute\n")
			os.Exit(1)
		}
		executor, err = NewExecutor(privKey, rpcURL, routerAddr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[arb4] ERROR: %v\n", err)
			os.Exit(1)
		}
		nonce, err := executor.FetchNonce()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[arb4] ERROR fetching nonce: %v\n", err)
			os.Exit(1)
		}
		executor.SetNonce(nonce)

		// Check WAVAX balance + allowance
		wavaxBal, err := executor.FetchERC20Balance(WAVAX)
		if err == nil && wavaxBal.Sign() > 0 {
			balF := new(big.Float).Quo(new(big.Float).SetInt(wavaxBal), new(big.Float).SetFloat64(1e18))
			fmt.Fprintf(os.Stderr, "[arb4] WAVAX balance: %s\n", balF.Text('f', 6))
		}
		allowance, err := executor.CheckAllowance(WAVAX)
		if err == nil {
			minAllowance := new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
			if allowance.Cmp(minAllowance) < 0 {
				fmt.Fprintf(os.Stderr, "[arb4] WAVAX allowance too low, approving...\n")
				txHash, err := executor.Approve(WAVAX, ls.BaseFee())
				if err != nil {
					fmt.Fprintf(os.Stderr, "[arb4] ERROR: approve failed: %v\n", err)
					os.Exit(1)
				}
				fmt.Fprintf(os.Stderr, "[arb4] approve tx: %s\n", txHash.Hex())
				time.Sleep(3 * time.Second)
				nonce, _ = executor.FetchNonce()
				executor.SetNonce(nonce)
			}
		}
		fmt.Fprintf(os.Stderr, "[arb4] executor ready\n")
	}

	// Warmup: build all pool quoters
	ls.RLock()
	for i := range pools {
		pm.Get(pools[i].Address)
	}
	ls.RUnlock()
	fmt.Fprintf(os.Stderr, "[arb4] ready. Waiting for blocks...\n")

	// Block processing loop
	type blockEvent struct {
		block, timestamp, baseFee, gasLimit uint64
		entries                             [][2]string
	}
	blockCh := make(chan blockEvent, 4)

	ls.SetOnBlock(func(ls *statedb.LiveState, entries [][2]string) {
		select {
		case blockCh <- blockEvent{ls.Block(), ls.Timestamp(), ls.BaseFee(), ls.GasLimit(), entries}:
		default:
			fmt.Fprintf(os.Stderr, "[arb4] WARNING: dropped block %d\n", ls.Block())
		}
	})

	for bi := range blockCh {
		pm.SetBlockTimestamp(bi.timestamp)

		// Identify dirty pools from state diff
		dirtySet := make(map[common.Address]bool)
		for _, entry := range bi.entries {
			key := entry[0]
			if strings.HasPrefix(key, "s:") {
				parts := strings.SplitN(key, ":", 3)
				if len(parts) == 3 {
					addr := common.HexToAddress(parts[1])
					slot := common.HexToHash(parts[2])
					for _, pa := range pm.InvalidateBySlot(addr, slot) {
						dirtySet[pa] = true
					}
				}
			}
		}

		if len(dirtySet) == 0 {
			continue
		}

		cfg := statedb.EVMConfig{
			BlockNumber: bi.block,
			Timestamp:   bi.timestamp,
			ChainID:     43114,
			BaseFee:     bi.baseFee,
			GasLimit:    bi.gasLimit,
		}
		gasPrice := bi.baseFee // use base fee as gas price estimate

		t0 := time.Now()

		ls.RLock()

		// ── Phase 1: Exclusion BFS — discover diverse pool set ──
		t1 := time.Now()
		discoveredPools := make(map[uint16]bool)
		for _, hub := range hubs {
			amounts := probeAmounts[hub]
			for _, amt := range amounts {
				discovered := DiscoverPools(pm, adj, pools, stateWithOverrides, cfg, routerAddr, sender, hub, amt, maxHops)
				for _, idx := range discovered {
					discoveredPools[idx] = true
				}
			}
		}
		phase1Time := time.Since(t1)

		if len(discoveredPools) == 0 {
			ls.RUnlock()
			fmt.Fprintf(os.Stderr, "[arb4] block=%d dirty=%d | no pools discovered | phase1=%v\n",
				bi.block, len(dirtySet), phase1Time.Round(time.Microsecond))
			continue
		}

		// Build reduced adjacency
		reducedAdj, reducedPools := buildReducedAdjacency(pools, discoveredPools, registry)

		// ── Phase 1.5: Pricing wave ──
		t15 := time.Now()
		prices := PricingWave(pm, reducedAdj, reducedPools, WAVAX)
		pricingTime := time.Since(t15)

		// ── Phase 2: EVM BFS on reduced pool set ──
		t2 := time.Now()
		var bestResult *CycleResult
		for _, hub := range hubs {
			amounts := probeAmounts[hub]
			for _, amt := range amounts {
				result := EVMBFS(reducedAdj, reducedPools, stateWithOverrides, cfg, routerAddr, sender, hub, amt, maxHops, gasPrice, prices)
				if result != nil && result.Profit.Sign() > 0 {
					if bestResult == nil || result.Profit.Cmp(&bestResult.Profit) > 0 {
						bestResult = result
					}
				}
			}
		}
		phase2Time := time.Since(t2)

		// ── Phase 3: EVM sizing — ternary search on winning cycle ──
		var finalResult *CycleResult
		t3 := time.Now()
		if bestResult != nil {
			sized := EVMSizing(bestResult.Steps, stateWithOverrides, cfg, routerAddr, sender, bestResult.Hub, gasPrice)
			if sized != nil && sized.Profit.Cmp(&bestResult.Profit) > 0 {
				finalResult = sized
			} else {
				finalResult = bestResult
			}
		}
		phase3Time := time.Since(t3)

		ls.RUnlock()

		totalTime := time.Since(t0)
		fmt.Fprintf(os.Stderr, "[arb4] block=%d dirty=%d pools_discovered=%d | phase1=%v pricing=%v phase2=%v phase3=%v total=%v",
			bi.block, len(dirtySet), len(discoveredPools),
			phase1Time.Round(time.Microsecond),
			pricingTime.Round(time.Microsecond),
			phase2Time.Round(time.Microsecond),
			phase3Time.Round(time.Microsecond),
			totalTime.Round(time.Microsecond),
		)

		if finalResult != nil && finalResult.Profit.Sign() > 0 {
			profitF := float64FromU256(&finalResult.Profit) / 1e18
			inputF := float64FromU256(&finalResult.AmountIn) / 1e18
			fmt.Fprintf(os.Stderr, " | %s %d-hop profit=%.6f in=%.4f gas=%d",
				hubName(finalResult.Hub), len(finalResult.Steps), profitF, inputF, finalResult.GasUsed)

			// JSON output
			poolAddrs := make([]string, len(finalResult.Steps))
			for i, s := range finalResult.Steps {
				poolAddrs[i] = s.Pool.Hex()
			}
			out, _ := json.Marshal(map[string]interface{}{
				"type":       "opportunity",
				"block":      bi.block,
				"hub":        hubName(finalResult.Hub),
				"profit_wei": finalResult.Profit.Dec(),
				"hops":       len(finalResult.Steps),
				"pools":      poolAddrs,
				"amount":     finalResult.AmountIn.Dec(),
				"gas":        finalResult.GasUsed,
			})
			fmt.Println(string(out))

			// ── Phase 4: Execute ──
			if executor != nil && finalResult.Profit.Sign() > 0 {
				txHash, err := executor.Execute(finalResult, bi.baseFee)
				if err != nil {
					fmt.Fprintf(os.Stderr, " | exec error: %v", err)
				} else {
					fmt.Fprintf(os.Stderr, " | EXECUTED tx=%s", txHash.Hex()[:14])
					execOut, _ := json.Marshal(map[string]interface{}{
						"type":   "tx_sent",
						"block":  bi.block,
						"txHash": txHash.Hex(),
					})
					fmt.Println(string(execOut))
				}
			}
		}
		fmt.Fprintf(os.Stderr, "\n")
	}
}

// buildReducedAdjacency creates a filtered adjacency and pool slice from discovered pool indices.
func buildReducedAdjacency(allPools []pf.Pool, discovered map[uint16]bool, registry *formulas.Registry) (map[common.Address][]pf.PoolEdge, []pf.Pool) {
	adj := make(map[common.Address][]pf.PoolEdge)
	for i := range allPools {
		if !discovered[uint16(i)] {
			continue
		}
		p := &allPools[i]
		if len(p.Tokens) < 2 {
			continue
		}
		idx := uint16(i)
		for ti := range p.Tokens {
			for tj := range p.Tokens {
				if ti != tj {
					adj[p.Tokens[ti]] = append(adj[p.Tokens[ti]], pf.PoolEdge{PoolIdx: idx, TokenOut: p.Tokens[tj], TokenIn: p.Tokens[ti]})
				}
			}
		}
	}
	return adj, allPools
}

func float64FromU256(v *uint256.Int) float64 {
	if v.IsZero() {
		return 0
	}
	b := v.ToBig()
	f, _ := new(big.Float).SetInt(b).Float64()
	return f
}
