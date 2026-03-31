package main

import (
	"fmt"
	"os"
	"time"

	"defi-toolbox/formulas"
	poolcollector "defi-toolbox/pool-collector"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

func main() {
	registry := formulas.LoadEmbeddedRegistry()
	pools := poolcollector.EmbeddedPools(1000)

	ls, err := statedb.Connect("ws://localhost:7449/live")
	if err != nil {
		panic(err)
	}
	state := ls.State()

	stateReader := func(addr common.Address, slot common.Hash) common.Hash {
		return state.GetState(addr, slot)
	}
	pm := formulas.NewPoolManager(registry, stateReader)
	for _, p := range pools {
		if len(p.Tokens) >= 2 {
			pm.SetPoolTokens(p.Address, p.Tokens[0], p.Tokens[1])
		}
		pm.SetPoolType(p.Address, p.PoolType, p.Dex)
	}
	pm.SetBlockTimestamp(ls.Timestamp())

	// Warm up all pools
	amt := uint256.NewInt(1e18)
	for _, p := range pools {
		pm.QuoteBypassQuoteCache(p.Address, amt, true)
		pm.QuoteBypassQuoteCache(p.Address, amt, false)
	}
	fmt.Fprintf(os.Stderr, "Warmed %d pools\n", len(pools))

	// Benchmark: quote all pools × 2 dirs × 5 sizes
	sizes := [5]*uint256.Int{
		uint256.NewInt(1e15),
		uint256.NewInt(1e16),
		uint256.NewInt(1e17),
		uint256.NewInt(1e18),
		new(uint256.Int).Mul(uint256.NewInt(10), uint256.NewInt(1e18)),
	}

	total := 0
	t0 := time.Now()
	for round := 0; round < 10; round++ {
		for _, p := range pools {
			for _, s := range sizes {
				pm.QuoteBypassQuoteCache(p.Address, s, true)
				pm.QuoteBypassQuoteCache(p.Address, s, false)
				total += 2
			}
		}
	}
	elapsed := time.Since(t0)
	qps := float64(total) / elapsed.Seconds()
	usPerQuote := float64(elapsed.Microseconds()) / float64(total)
	fmt.Fprintf(os.Stderr, "%d quotes in %v = %.0f quotes/sec = %.1fμs/quote\n", total, elapsed, qps, usPerQuote)
}
