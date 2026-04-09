// Profile GreedyFast at 100 chunks for a single pair.
package main

import (
	"flag"
	"fmt"
	"math/big"
	"os"
	"runtime/pprof"

	"defi-toolbox/pathfinder/splitter"
	"defi-toolbox/quoter"
	"defi-toolbox/statedb"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

func main() {
	stateServer := flag.String("state-server", "ws://localhost:7449/live", "state server WebSocket URL")
	cpuprofile := flag.String("cpuprofile", "/tmp/gfast100.prof", "cpu profile output")
	flag.Parse()

	ls, err := statedb.Connect(*stateServer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect failed: %v\n", err)
		os.Exit(1)
	}

	q := quoter.NewQuoter(ls, 2000, 3)
	q.StartBlockLoop()

	ready := make(chan struct{})
	q.SetOnBlock(func(block, timestamp uint64) {
		select {
		case ready <- struct{}{}:
		default:
		}
	})
	<-ready

	ls.RLock()
	defer ls.RUnlock()

	tokenIn := common.HexToAddress("0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7")  // WAVAX
	tokenOut := common.HexToAddress("0x9702230A8Ea53601f5cD2dc00fDBc13d4dF4A8c7") // USDT
	amount, _ := new(big.Int).SetString("50000000000000000000000", 10)              // 50000 WAVAX
	fullAmount := new(uint256.Int)
	fullAmount.SetFromBig(amount)

	cfg := ls.EVMConfig()
	params := &splitter.Params{
		PM: q.PM(), BasePM: q.PM(), Adj: q.Adj(), Pools: q.Pools(),
		State: q.StateWithOverrides(), EVMConfig: cfg,
		RouterAddr: q.RouterAddr(), Sender: q.Sender(),
		TokenIn: tokenIn, TokenOut: tokenOut, MaxHops: q.MaxHops(),
	}

	// Warm up
	splitter.GreedyFast(params, fullAmount, 10)

	// Profile
	f, _ := os.Create(*cpuprofile)
	pprof.StartCPUProfile(f)
	result := splitter.GreedyFast(params, fullAmount, 100)
	pprof.StopCPUProfile()
	f.Close()

	if result != nil {
		fmt.Printf("total: %s  legs=%d  time=%dμs\n", result.Total.Dec(), len(result.Legs), result.ElapsedUs)
	}
}
