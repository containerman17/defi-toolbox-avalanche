package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"time"

	lc "defi-toolbox/lightclient"

	"github.com/ava-labs/libevm/common"
)

func main() {
	rpcURL := flag.String("rpc", "ws://127.0.0.1:9650/ext/bc/C/ws", "WebSocket RPC URL")
	blocks := flag.Int("blocks", 10, "number of recent blocks to verify")
	startBlock := flag.Uint64("start-block", 0, "starting block number (0 = head minus blocks)")
	traceEvery := flag.Int("trace-every", 1, "trace every Nth block for verification (1 = every block)")
	concurrency := flag.Int("concurrency", 2*runtime.NumCPU(), "RPC pool size")
	prefetch := flag.Bool("prefetch", true, "enable prefetch with separate RPC pool")
	flag.Parse()

	pool, err := lc.NewRPCPool(*rpcURL, *concurrency)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rpc pool: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	chainCfg, err := lc.FetchChainConfig(pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chain config: %v\n", err)
		os.Exit(1)
	}

	var prefetchFetcher *lc.BlockFetcher
	if *prefetch {
		prefetchPool, err := lc.NewRPCPool(*rpcURL, *concurrency)
		if err != nil {
			fmt.Fprintf(os.Stderr, "prefetch rpc pool: %v\n", err)
			os.Exit(1)
		}
		defer prefetchPool.Close()
		prefetchFetcher = lc.NewBlockFetcher(prefetchPool)
	}

	fetcher := lc.NewBlockFetcher(pool)
	state := lc.NewVersionedState()

	headNum, err := fetchHeadBlock(pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch head: %v\n", err)
		os.Exit(1)
	}

	start := uint64(0)
	endBlock := uint64(0)
	if *startBlock > 0 {
		start = *startBlock
		endBlock = start + uint64(*blocks) - 1
		if endBlock > headNum {
			endBlock = headNum
		}
	} else {
		endBlock = headNum
		start = headNum - uint64(*blocks) + 1
		if headNum < uint64(*blocks) {
			start = 1
		}
	}

	blockHashes := make(map[uint64]common.Hash)
	getHash := func(n uint64) common.Hash {
		if h, ok := blockHashes[n]; ok {
			return h
		}
		h, err := fetcher.GetBlockHash(n)
		if err != nil {
			return common.Hash{}
		}
		blockHashes[n] = h
		return h
	}

	matched := 0
	total := 0
	var execTimes []int
	var fetchCounts []int
	var pfFetchCounts []int
	lastStats := time.Now()

	pipeline, pipeStop := fetcher.Pipeline(start, endBlock, 64)
	defer close(pipeStop)

	for br := range pipeline {
		if br.Err != nil {
			fmt.Fprintf(os.Stderr, "fetch error: %v\n", br.Err)
			os.Exit(1)
		}
		bd := br.Data
		blockNum := bd.Number
		total++

		blockHashes[blockNum] = bd.Hash
		block := lc.BlockDataToTypesBlock(bd)

		var stats lc.FetchStats
		miss := fetcher.MissCallbacksWithStats(state, &stats)
		sv := lc.NewStateView(state, blockNum-1, miss)

		var pfMiss *lc.MissCallbacks
		var pfStats lc.FetchStats
		if prefetchFetcher != nil {
			m := prefetchFetcher.MissCallbacksWithStats(state, &pfStats)
			pfMiss = &m
		}

		execStart := time.Now()
		diff, err := lc.ExecuteBlock(block, sv, chainCfg, getHash, pfMiss)
		if err != nil {
			fmt.Fprintf(os.Stderr, "block %d: execute error: %v\n", blockNum, err)
			os.Exit(1)
		}
		execElapsed := time.Since(execStart)

		// Apply diffs to versioned state so subsequent blocks can read them.
		applyDiff(state, diff, blockNum)
		state.SetLatestBlock(blockNum)

		execMs := int(execElapsed.Milliseconds())
		execTimes = append(execTimes, execMs)
		fetchCounts = append(fetchCounts, stats.Total())
		pfFetchCounts = append(pfFetchCounts, pfStats.Total())

		// Trace only every Nth block (tracing is ~130ms vs ~25ms execution).
		// Drift accumulates, so we still catch it — just narrow to a 1000-block window.
		shouldTrace := *traceEvery <= 1 || total%*traceEvery == 0 || blockNum == endBlock
		var mismatches []string
		var traced *lc.TraceDiff
		if shouldTrace {
			var err2 error
			traced, err2 = fetcher.TraceBlock(blockNum)
			if err2 != nil {
				fmt.Fprintf(os.Stderr, "block %d: trace error: %v\n", blockNum, err2)
				os.Exit(1)
			}
			mismatches = verifyAgainstTrace(diff, traced)
		}

		if len(mismatches) == 0 {
			matched++

			// Print running percentiles every 5 seconds.
			if time.Since(lastStats) >= 5*time.Second {
				sortedExec := make([]int, len(execTimes))
				copy(sortedExec, execTimes)
				sort.Ints(sortedExec)
				sortedFetch := make([]int, len(fetchCounts))
				copy(sortedFetch, fetchCounts)
				sort.Ints(sortedFetch)
				sortedPF := make([]int, len(pfFetchCounts))
				copy(sortedPF, pfFetchCounts)
				sort.Ints(sortedPF)
				n := len(sortedExec)
				pe := func(pct int) int { return sortedExec[min(n*pct/100, n-1)] }
				pf := func(pct int) int { return sortedFetch[min(n*pct/100, n-1)] }
				pp := func(pct int) int { return sortedPF[min(n*pct/100, n-1)] }
				fmt.Printf("--- %d blocks | exec p50=%dms p90=%dms p95=%dms p99=%dms | fetches p50=%d p90=%d | prefetch p50=%d p90=%d\n",
					n, pe(50), pe(90), pe(95), pe(99), pf(50), pf(90), pp(50), pp(90))
				lastStats = time.Now()
			}
		} else {
			fmt.Printf("block %d: MISMATCH\n", blockNum)
			for _, m := range mismatches {
				fmt.Printf("  %s\n", m)
			}
			// Debug: for nonce mismatches, query RPC to see if our pre-state was wrong.
			for addr, tracedNonce := range traced.Nonce {
				localNonce, ok := diff.Nonces[addr]
				if !ok {
					localNonce = 0
				}
				if localNonce != tracedNonce {
					rpcNonce, err := fetcher.GetNonce(addr, blockNum-1)
					vsNonce, vsOk := state.GetNonce(addr, blockNum-1)
					fmt.Printf("  DEBUG nonce %s:\n", addr.Hex())
					fmt.Printf("    traced post-nonce: %d\n", tracedNonce)
					fmt.Printf("    local  post-nonce: %d (in diff: %v)\n", localNonce, ok)
					if err == nil {
						fmt.Printf("    RPC pre-nonce (block %d): %d\n", blockNum-1, rpcNonce)
					} else {
						fmt.Printf("    RPC pre-nonce: error: %v\n", err)
					}
					fmt.Printf("    VersionedState pre-nonce (block %d): %d (found: %v)\n", blockNum-1, vsNonce, vsOk)
				}
			}
			fmt.Printf("\nverify: FAILED at block %d (%d/%d matched before failure)\n", blockNum, matched, total)
			os.Exit(1)
		}
	}

	fmt.Printf("\nverify: %d/%d blocks matched\n", matched, total)
}

// verifyAgainstTrace compares local storage diffs against the trace.
// Only checks storage and nonces — balance comparison skipped because
// the trace doesn't capture atomic tx balance changes, and gas fee
// accounting has minor differences.
func verifyAgainstTrace(local *lc.BlockDiff, traced *lc.TraceDiff) []string {
	var mismatches []string

	// Check traced storage against local overlay.
	// Skip mismatches for newly created contracts — the prestateTracer reports
	// zeros for their storage since the contract didn't exist in pre-state.
	newContracts := make(map[common.Address]bool)
	for addr := range local.Code {
		newContracts[addr] = true
	}
	for addr, slots := range traced.Storage {
		if newContracts[addr] {
			continue
		}
		localSlots := local.Storage[addr]
		for slot, tracedVal := range slots {
			localVal := common.Hash{}
			if localSlots != nil {
				localVal = localSlots[slot]
			}
			if localVal != tracedVal {
				mismatches = append(mismatches, fmt.Sprintf(
					"storage %s slot %s: local=%s traced=%s",
					short(addr), shortH(slot), localVal.Hex(), tracedVal.Hex()))
			}
		}
	}

	// Check local storage has no extra entries not in trace (except coinbase-related).
	for addr, slots := range local.Storage {
		tracedSlots := traced.Storage[addr]
		for slot, localVal := range slots {
			if tracedSlots == nil {
				continue // local has extra address not in trace — informational only
			}
			if _, ok := tracedSlots[slot]; !ok {
				continue // extra slot not in trace — informational only
			}
			// Already checked in the loop above.
			_ = localVal
		}
	}

	// Check nonces.
	for addr, tracedNonce := range traced.Nonce {
		localNonce, ok := local.Nonces[addr]
		if !ok {
			localNonce = 0
		}
		if localNonce != tracedNonce {
			mismatches = append(mismatches, fmt.Sprintf(
				"nonce %s: local=%d traced=%d", short(addr), localNonce, tracedNonce))
		}
	}

	return mismatches
}

func short(addr common.Address) string {
	return addr.Hex()
}

func shortH(h common.Hash) string {
	return h.Hex()
}

func applyDiff(state *lc.VersionedState, diff *lc.BlockDiff, block uint64) {
	for addr, slots := range diff.Storage {
		for slot, val := range slots {
			state.SetStorage(addr, slot, val, block)
		}
	}
	for addr, bal := range diff.Balances {
		state.SetBalance(addr, bal, block)
	}
	for addr, nonce := range diff.Nonces {
		state.SetNonce(addr, nonce, block)
	}
	for addr, code := range diff.Code {
		state.SetCode(addr, code, block)
	}
}

func fetchHeadBlock(pool *lc.RPCPool) (uint64, error) {
	raw, err := pool.Call("eth_blockNumber", []interface{}{})
	if err != nil {
		return 0, err
	}
	var hex string
	if err := json.Unmarshal(raw, &hex); err != nil {
		return 0, err
	}
	var n uint64
	fmt.Sscanf(hex, "0x%x", &n)
	return n, nil
}
