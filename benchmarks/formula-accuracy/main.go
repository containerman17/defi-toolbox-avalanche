// formula-accuracy — Compares formula quotes against EVM ground truth.
//
// For each pool in the registry, quotes via formula and via EVM (router
// debugSwapSingle), reports match/mismatch/overquote stats per pool type.
// Runs against one or more blocks starting from the router deployment block,
// spaced 10 000 blocks apart. Uses the LightClient in fixed-block mode so
// fetched state is persisted as snapshots and reused across runs.
//
// Usage:
//
//	go run ./benchmarks/formula-accuracy/ [--blocks 1] [--limit 4000]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	router "defi-toolbox/contracts"
	"defi-toolbox/formulas"
	lc "defi-toolbox/lightclient"
	"defi-toolbox/pathfinder"
	poolcollector "defi-toolbox/tools/pool-collector"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

var DUMMY_SENDER = common.HexToAddress("0x000000000000000000000000000000000000dEaD")
var debugEVM = false

const (
	defaultPoolLimit = 4000
	blockSpacing     = 10_000
)

var typeNames = map[int]string{
	0:  "uniswap_v3",
	1:  "algebra",
	2:  "lfj_v1",
	3:  "lfj_v2",
	4:  "dodo",
	5:  "woofi_v2",
	6:  "balancer_v3",
	7:  "pharaoh_v1",
	8:  "v2",
	9:  "uniswap_v4",
	10: "erc4626",
	11: "balancer_v3_buffered",
	12: "wombat",
	13: "platypus",
	14: "woopp_v2",
	15: "transfer_from",
	16: "balancer_v2",
	17: "cavalre",
	18: "kyber_dmm",
	19: "synapse",
	20: "trident",
}

type typeStats struct {
	Quotes     int
	Match      int
	Overquote  int // formula > EVM — never acceptable
	Underquote int // formula < EVM — safe, missed opportunity
	Zero       int // both returned zero
}

func (s *typeStats) add(o *typeStats) {
	s.Quotes += o.Quotes
	s.Match += o.Match
	s.Overquote += o.Overquote
	s.Underquote += o.Underquote
	s.Zero += o.Zero
}

type quoteJob struct {
	pool     pathfinder.Pool
	tokenIn  common.Address
	tokenOut common.Address
	amount   *uint256.Int
}

type blockResult struct {
	block      uint64
	byType     map[int]*typeStats
	overquotes int
	jobs       int
	elapsed    time.Duration
}

func main() {
	rpcURL := flag.String("rpc", "ws://127.0.0.1:9650/ext/bc/C/ws", "WebSocket RPC URL")
	concurrency := flag.Int("concurrency", 2*runtime.NumCPU(), "RPC pool size")
	poolLimit := flag.Int("limit", defaultPoolLimit, "pool limit (default 4000, 0 = all)")
	nBlocks := flag.Int("blocks", 10, "number of blocks to test, spaced 10k apart from deployment")
	dataDir := flag.String("data-dir", "benchmarks/formula-accuracy/.lightclient", "lightclient snapshot directory")
	flag.Parse()

	// Load pool and formula data (shared across all blocks).
	pools := poolcollector.EmbeddedPools(*poolLimit)
	registry := formulas.LoadEmbeddedRegistry()
	tokenAmounts := formulas.LoadEmbeddedTokenAmounts()

	// Register V4 pools.
	for _, p := range pools {
		if p.PoolType != 9 || p.ExtraData == "" {
			continue
		}
		var poolIdHex string
		var fee uint32
		var tickSpacing int32
		var hooks common.Address
		for _, kv := range strings.Split(p.ExtraData, ",") {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 {
				continue
			}
			switch parts[0] {
			case "id":
				poolIdHex = parts[1]
			case "fee":
				var f int
				fmt.Sscanf(parts[1], "%d", &f)
				fee = uint32(f)
			case "ts":
				var t int
				fmt.Sscanf(parts[1], "%d", &t)
				tickSpacing = int32(t)
			case "hooks":
				hooks = common.HexToAddress(parts[1])
			}
		}
		if poolIdHex != "" && tickSpacing != 0 {
			var poolId [32]byte
			copy(poolId[:], common.FromHex(poolIdHex))
			formulas.RegisterV4Pool(strings.ToLower(p.Address.Hex()), poolId, tickSpacing, fee, 0, hooks)
		}
	}

	// Collect all tokens for overrides.
	allTokens := make([]common.Address, 0)
	tokenSeen := make(map[common.Address]bool)
	for _, p := range pools {
		for _, t := range p.Tokens {
			if !tokenSeen[t] {
				tokenSeen[t] = true
				allTokens = append(allTokens, t)
			}
		}
	}

	// Build jobs (shared across all blocks).
	var jobs []quoteJob
	jobPools := make(map[common.Address]struct{})
	for _, p := range pools {
		if _, known := registry.GetFormulaID(p.Address); !known {
			continue
		}
		if len(p.Tokens) < 2 {
			continue
		}
		addedJob := false
		for ti, tok := range p.Tokens {
			if amt, ok := tokenAmounts[tok]; ok {
				for tj := range p.Tokens {
					if ti != tj {
						jobs = append(jobs, quoteJob{p, tok, p.Tokens[tj], amt})
						addedJob = true
					}
				}
				break
			}
		}
		if addedJob {
			jobPools[p.Address] = struct{}{}
		}
	}

	fmt.Fprintf(os.Stderr, "[bench] %d quote jobs across %d eligible pools (%d input pools)\n",
		len(jobs), len(jobPools), len(pools))

	// Run each block.
	baseBlock := router.DeployedBlock
	var results []blockResult
	for i := 0; i < *nBlocks; i++ {
		blockNum := baseBlock + uint64(i)*blockSpacing
		fmt.Fprintf(os.Stderr, "\n[bench] === block %d (%d/%d) ===\n", blockNum, i+1, *nBlocks)
		r := runBlock(blockNum, *rpcURL, *dataDir, *concurrency, pools, registry, allTokens, jobs)
		results = append(results, r)
		fmt.Fprintf(os.Stderr, "[bench] block %d: %d jobs, %d overquotes, %v\n",
			blockNum, r.jobs, r.overquotes, r.elapsed.Round(time.Millisecond))
	}

	// Aggregate across all blocks.
	totByType := make(map[int]*typeStats)
	totalOverquotes := 0
	for _, r := range results {
		totalOverquotes += r.overquotes
		for pt, st := range r.byType {
			if totByType[pt] == nil {
				totByType[pt] = &typeStats{}
			}
			totByType[pt].add(st)
		}
	}

	// Print results.
	totalQuotes, totalMatch, totalOver, totalUnder, totalZero := 0, 0, 0, 0, 0
	fmt.Printf("%-15s %6s %6s %6s %6s %6s\n", "type", "quotes", "match", "over", "under", "zero")
	fmt.Printf("%-15s %6s %6s %6s %6s %6s\n", "----", "------", "-----", "----", "-----", "----")
	poolTypes := make([]int, 0, len(totByType))
	for pt := range totByType {
		poolTypes = append(poolTypes, pt)
	}
	sort.Ints(poolTypes)
	for _, pt := range poolTypes {
		st := totByType[pt]
		name := typeNames[pt]
		if name == "" {
			name = fmt.Sprintf("type_%d", pt)
		}
		fmt.Printf("%-15s %6d %6d %6d %6d %6d\n", name, st.Quotes, st.Match, st.Overquote, st.Underquote, st.Zero)
		totalQuotes += st.Quotes
		totalMatch += st.Match
		totalOver += st.Overquote
		totalUnder += st.Underquote
		totalZero += st.Zero
	}
	fmt.Printf("%-15s %6d %6d %6d %6d %6d\n", "TOTAL", totalQuotes, totalMatch, totalOver, totalUnder, totalZero)
	fmt.Printf("\nblocks=%d input_pools=%d eligible_pools=%d quote_jobs=%d\n",
		len(results), len(pools), len(jobPools), totalQuotes)
	fmt.Printf("exact=%.2f%% over=%.2f%% under=%.2f%% zero=%.2f%% non_zero=%.2f%%\n",
		pct(totalMatch, totalQuotes), pct(totalOver, totalQuotes), pct(totalUnder, totalQuotes),
		pct(totalZero, totalQuotes), pct(totalQuotes-totalZero, totalQuotes))

	if totalOver > 0 {
		fmt.Fprintf(os.Stderr, "\nWARNING: %d overquotes detected!\n", totalOver)
	}
}

// runBlock runs all quote jobs against a single block using a LightClient in
// fixed-block mode. State is loaded from / saved to a snapshot automatically.
func runBlock(
	blockNum uint64,
	rpcURL, dataDir string,
	concurrency int,
	pools []pathfinder.Pool,
	registry *formulas.Registry,
	allTokens []common.Address,
	jobs []quoteJob,
) blockResult {
	client, err := lc.New(lc.Config{
		RPCURL:      rpcURL,
		DataDir:     dataDir,
		FixedBlock:  blockNum,
		Concurrency: concurrency,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "lightclient new: %v\n", err)
		os.Exit(1)
	}
	client.DebugLogging = false
	defer client.Close()

	if err := client.Start(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "lightclient start: %v\n", err)
		os.Exit(1)
	}

	timestamp, err := client.BlockTimestamp(0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "block timestamp: %v\n", err)
		os.Exit(1)
	}

	// Formula reader backed by LightClient state.
	formulaSV, err := client.StateView(0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "state view: %v\n", err)
		os.Exit(1)
	}
	reader := func(addr common.Address, slot common.Hash) common.Hash {
		return formulaSV.GetState(addr, slot)
	}
	pm := formulas.NewPoolManager(registry, reader)
	pm.SetBlockTimestamp(timestamp)
	// Wire EVMCaller so formulas that need view calls (LFJ V2 rebasing surplus
	// via balanceOf, Balancer V3 rate providers, wombat ggAVAX oracle) get real
	// on-chain values instead of silently falling through to defaults.
	pm.SetEVMCaller(func(to common.Address, data []byte) ([]byte, bool) {
		ret, _, err := client.Call(lc.CallMsg{
			From: DUMMY_SENDER,
			To:   &to,
			Data: data,
		}, 0)
		return ret, err == nil
	})
	for _, p := range pools {
		if len(p.Tokens) >= 2 {
			pm.SetPoolTokens(p.Address, p.Tokens...)
		}
		pm.SetPoolType(p.Address, p.PoolType, p.Dex)
	}

	t0 := time.Now()

	// Phase 1: parallel EVM quotes. Each call populates VersionedState via RPC
	// miss callbacks, warming the cache for the formula phase. Concurrency
	// matches the RPC pool so workers stay busy.
	evmOuts := make([]*uint256.Int, len(jobs))
	var done atomic.Int64
	workers := concurrency
	if workers > len(jobs) {
		workers = len(jobs)
	}
	jobCh := make(chan int, len(jobs))
	for i := range jobs {
		jobCh <- i
	}
	close(jobCh)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobCh {
				j := jobs[i]
				evmOuts[i] = evmQuote(client, allTokens,
					j.pool.Address, j.pool.PoolType, j.tokenIn, j.tokenOut, j.amount, j.pool.ExtraData)
				if n := done.Add(1); n%500 == 0 {
					fmt.Fprintf(os.Stderr, "  EVM %d/%d\n", n, len(jobs))
				}
			}
		}()
	}
	wg.Wait()
	evmElapsed := time.Since(t0)
	fmt.Fprintf(os.Stderr, "  EVM phase: %v\n", evmElapsed.Round(time.Millisecond))

	// Phase 2: sequential formula quotes + comparison. State is warm.
	tForm := time.Now()
	byType := make(map[int]*typeStats)
	overquotes := 0
	for i, job := range jobs {
		evmOut := evmOuts[i]
		fOut := pm.Quote(job.pool.Address, job.amount, job.tokenIn, job.tokenOut)

		st := byType[job.pool.PoolType]
		if st == nil {
			st = &typeStats{}
			byType[job.pool.PoolType] = st
		}
		st.Quotes++

		evmIsZero := evmOut == nil || evmOut.IsZero()
		fIsZero := fOut.IsZero()

		if evmIsZero && fIsZero {
			st.Zero++
			st.Match++
		} else if evmIsZero || fIsZero {
			if !fIsZero {
				st.Overquote++
				overquotes++
				fmt.Printf("OVERQUOTE block=%d %s type=%d %s→%s evm=0 formula=%s\n",
					blockNum, job.pool.Address.Hex()[:10], job.pool.PoolType,
					job.tokenIn.Hex()[:10], job.tokenOut.Hex()[:10],
					fOut.Dec())
			} else {
				st.Underquote++
			}
		} else if fOut.Eq(evmOut) {
			st.Match++
		} else if fOut.Gt(evmOut) {
			st.Overquote++
			overquotes++
			fmt.Printf("OVERQUOTE block=%d %s type=%d %s→%s evm=%s formula=%s\n",
				blockNum, job.pool.Address.Hex()[:10], job.pool.PoolType,
				job.tokenIn.Hex()[:10], job.tokenOut.Hex()[:10],
				evmOut.Dec(), fOut.Dec())
		} else {
			st.Underquote++
		}
	}
	fmt.Fprintf(os.Stderr, "  formula phase: %v\n", time.Since(tForm).Round(time.Millisecond))

	return blockResult{
		block:      blockNum,
		byType:     byType,
		overquotes: overquotes,
		jobs:       len(jobs),
		elapsed:    time.Since(t0),
	}
}

func evmQuote(client *lc.LightClient, allTokens []common.Address,
	poolAddr common.Address, poolType int,
	tokenIn, tokenOut common.Address, amount *uint256.Int, extraData string,
) *uint256.Int {
	calldata := pathfinder.EncodeSwapSingleWithExtra(poolAddr, poolType, tokenIn, tokenOut, amount, extraData)
	to := router.DeployedRouter
	ret, _, err := client.DirectCallWithState(lc.CallMsg{
		From: DUMMY_SENDER,
		To:   &to,
		Data: calldata,
	}, 0, func(sv *lc.StateView) {
		router.ApplyTokenOverrides(sv, DUMMY_SENDER, router.DeployedRouter, allTokens)
	})
	if err != nil {
		if debugEVM {
			fmt.Fprintf(os.Stderr, "  EVM err: %v (pool=%s)\n", err, poolAddr.Hex()[:10])
		}
		return uint256.NewInt(0)
	}
	if len(ret) < 32 {
		return uint256.NewInt(0)
	}
	var out uint256.Int
	out.SetBytes(ret[:32])
	if out.Bytes32()[0]&0x80 != 0 {
		return uint256.NewInt(0)
	}
	return &out
}

func pct(numer, denom int) float64 {
	if denom == 0 {
		return 0
	}
	return 100 * float64(numer) / float64(denom)
}
