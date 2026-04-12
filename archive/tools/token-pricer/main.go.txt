// token-pricer discovers per-token amounts (~0.1 AVAX worth) by multi-wave
// EVM quoting through all pools, starting from WAVAX. Writes results to
// formulas/data/token_amounts.txt for use by the formula-accuracy benchmark.
//
// Uses local EVM (debugSwapSingle via router contract) — not formulas — so
// the amounts are ground truth for formula validation.
//
// Usage: timeout 300 go run ./tools/token-pricer/
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	router "defi-toolbox/contracts"
	"defi-toolbox/pathfinder"
	"defi-toolbox/statedb"
	poolcollector "defi-toolbox/tools/pool-collector"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

var WAVAX = common.HexToAddress("0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7")
var ROUTER = router.DeployedRouter
var DUMMY_SENDER = common.HexToAddress("0x000000000000000000000000000000000000dEaD")

func main() {
	stateServerHost := flag.String("state-server", "localhost:7449", "state server host:port")
	poolLimit := flag.Int("pool-limit", 2000, "max pools to load")
	outputFile := flag.String("output", "formulas/data/token_amounts.txt", "output file")
	waves := flag.Int("waves", 4, "number of discovery waves")
	flag.Parse()

	// Connect at the deploy block (same block the benchmark uses)
	deployBlock := router.DeployedBlock
	stateServerURL := fmt.Sprintf("ws://%s/debug/%d", *stateServerHost, deployBlock)
	fmt.Fprintf(os.Stderr, "connecting to block %d...\n", deployBlock)
	ls, err := statedb.Connect(stateServerURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect: %v\n", err)
		os.Exit(1)
	}
	defer ls.Close()
	state := ls.State()
	cfg := ls.EVMConfig()
	fmt.Fprintf(os.Stderr, "connected, block=%d\n", ls.Block())

	// Load pools
	pools := poolcollector.EmbeddedPools(*poolLimit)
	fmt.Fprintf(os.Stderr, "loaded %d pools\n", len(pools))

	// Build state with router token overrides (debugSwapSingle needs router to hold tokens)
	overrides := router.BuildTokenOverrides(ROUTER, pools)
	baseWithOverrides := pathfinder.ApplyOverridesFlat(state, overrides)

	// EVM executor
	evmCtx := statedb.GetCachedContext(cfg)

	// EVM quote function: debugSwapSingle through the router
	evmQuote := func(pool *pathfinder.Pool, tokenIn, tokenOut common.Address, amountIn *uint256.Int) *uint256.Int {
		calldata := pathfinder.EncodeSwapSingleWithExtra(pool.Address, pool.PoolType, tokenIn, tokenOut, amountIn, pool.ExtraData)
		cs := statedb.NewCallState(baseWithOverrides)
		ret, _, err := evmCtx.ExecuteWithCallState(cs, DUMMY_SENDER, ROUTER, calldata)
		if err != nil || cs.Err() != nil || len(ret) < 32 {
			return nil
		}
		out := new(uint256.Int).SetBytes(ret[:32])
		if out.IsZero() {
			return nil
		}
		return out
	}

	// Build adjacency: token → []edge
	type edge struct {
		poolIdx  int
		tokenOut common.Address
		dir      bool // zeroForOne
	}
	adj := make(map[common.Address][]edge)
	for i := range pools {
		p := &pools[i]
		if len(p.Tokens) < 2 {
			continue
		}
		adj[p.Tokens[0]] = append(adj[p.Tokens[0]], edge{i, p.Tokens[1], true})
		adj[p.Tokens[1]] = append(adj[p.Tokens[1]], edge{i, p.Tokens[0], false})
	}

	// Seed: 0.1 AVAX ≈ $1
	seedAmount := uint256.NewInt(100_000_000_000_000_000) // 1e17
	tokenPrice := make(map[common.Address]*uint256.Int)
	tokenPrice[WAVAX] = new(uint256.Int).Set(seedAmount)
	tokenPrice[common.Address{}] = new(uint256.Int).Set(seedAmount) // native AVAX for V4

	t0 := time.Now()
	totalQuotes := 0
	for wave := 0; wave < *waves; wave++ {
		waveQuotes := 0
		pricedBefore := len(tokenPrice)
		for token, edges := range adj {
			if _, priced := tokenPrice[token]; priced {
				continue
			}
			for _, e := range edges {
				knownPrice, priced := tokenPrice[e.tokenOut]
				if !priced {
					continue
				}
				pool := &pools[e.poolIdx]
				// Quote: knownToken → unknownToken
				// e.dir is the direction from `token` to `e.tokenOut`
				// We want the reverse: from e.tokenOut to token
				var tokenIn, tokenOut common.Address
				if e.dir {
					// edge is token→e.tokenOut (zeroForOne), reverse = e.tokenOut→token
					tokenIn = e.tokenOut
					tokenOut = token
				} else {
					tokenIn = e.tokenOut
					tokenOut = token
				}
				out := evmQuote(pool, tokenIn, tokenOut, knownPrice)
				waveQuotes++
				if out == nil {
					continue
				}
				existing, exists := tokenPrice[token]
				if !exists || out.Gt(existing) {
					tokenPrice[token] = out
				}
			}
		}
		totalQuotes += waveQuotes
		newTokens := len(tokenPrice) - pricedBefore
		fmt.Fprintf(os.Stderr, "wave %d: %d EVM quotes, +%d tokens (%d total)\n",
			wave+1, waveQuotes, newTokens, len(tokenPrice))
		if newTokens == 0 {
			break
		}
	}
	fmt.Fprintf(os.Stderr, "discovery done: %d tokens, %d quotes, %v\n",
		len(tokenPrice), totalQuotes, time.Since(t0).Round(time.Millisecond))

	// Count pool coverage
	skipped := 0
	quotable := 0
	for i := range pools {
		p := &pools[i]
		if len(p.Tokens) < 2 {
			continue
		}
		_, has0 := tokenPrice[p.Tokens[0]]
		_, has1 := tokenPrice[p.Tokens[1]]
		if has0 || has1 {
			quotable++
		} else {
			skipped++
		}
	}
	fmt.Fprintf(os.Stderr, "pool coverage: %d quotable, %d skipped (%.1f%%)\n",
		quotable, skipped, float64(quotable)/float64(quotable+skipped)*100)

	// Write output
	type entry struct {
		addr common.Address
		hex  string
	}
	var entries []entry
	for addr, amt := range tokenPrice {
		entries = append(entries, entry{addr, amt.Hex()})
	}
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].addr.Hex()) < strings.ToLower(entries[j].addr.Hex())
	})

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Token amounts equal to ~0.1 AVAX (~$1)\n"))
	sb.WriteString(fmt.Sprintf("# %d tokens, generated by tools/token-pricer at block %d\n", len(entries), ls.Block()))
	for _, e := range entries {
		sb.WriteString(fmt.Sprintf("%s:%s\n", strings.ToLower(e.addr.Hex()), e.hex))
	}

	if err := os.WriteFile(*outputFile, []byte(sb.String()), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *outputFile, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d tokens)\n", *outputFile, len(entries))
}
