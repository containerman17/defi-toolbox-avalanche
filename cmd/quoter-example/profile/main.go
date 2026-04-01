package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/pprof"
	"time"

	"defi-toolbox/cmd/quoter-example/shared"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
)

func main() {
	stateServer := flag.String("state-server", "ws://localhost:7449/live", "state server WebSocket URL")
	poolLimit := flag.Int("pool-limit", 2000, "max pools to load")
	maxHops := flag.Int("max-hops", 3, "max hops per route")
	rounds := flag.Int("rounds", 10, "number of quote rounds")
	cpuprofile := flag.String("cpuprofile", "", "write cpu profile to file")
	flag.Parse()

	ls, err := statedb.Connect(*stateServer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "connected, block=%d\n", ls.Block())

	q := shared.NewQuoter(ls, *poolLimit, *maxHops)
	fmt.Fprintf(os.Stderr, "quoter ready\n")

	req := shared.QuoteRequest{
		TokenIn:  "0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7", // WAVAX
		TokenOut: "0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E", // USDC
		AmountIn: "1000000000000000000",
	}

	// Warmup
	resp, _ := q.Quote(req)
	fmt.Fprintf(os.Stderr, "warmup done, out=%s\n", resp.Forward.AmountOut)

	// Profile
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "create profile: %v\n", err)
			os.Exit(1)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	t0 := time.Now()
	var lastOut string
	for i := 0; i < *rounds; i++ {
		resp, _ := q.Quote(req)
		lastOut = resp.Forward.AmountOut
	}
	elapsed := time.Since(t0)

	if *cpuprofile != "" {
		pprof.StopCPUProfile()
	}

	fmt.Fprintf(os.Stderr, "\n%d rounds in %dms (%.1fms/quote), last out=%s\n",
		*rounds, elapsed.Milliseconds(), float64(elapsed.Milliseconds())/float64(*rounds), lastOut)

	// Prevent dead-code elimination
	_ = common.Address{}
}
