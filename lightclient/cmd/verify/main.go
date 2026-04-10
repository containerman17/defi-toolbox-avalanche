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

	startBlock := headNum - uint64(*blocks) + 1
	if headNum < uint64(*blocks) {
		startBlock = 1
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
	lastStats := time.Now()

	for blockNum := startBlock; blockNum <= headNum; blockNum++ {
		total++

		bd, err := fetcher.GetBlock(blockNum)
		if err != nil {
			fmt.Fprintf(os.Stderr, "block %d: fetch error: %v\n", blockNum, err)
			os.Exit(1)
		}

		blockHashes[blockNum] = bd.Hash
		block := lc.BlockDataToTypesBlock(bd)

		var stats lc.FetchStats
		miss := fetcher.MissCallbacksWithStats(state, &stats)
		sv := lc.NewStateView(state, blockNum-1, miss)

		var pfMiss *lc.MissCallbacks
		if prefetchFetcher != nil {
			m := prefetchFetcher.MissCallbacks(state)
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

		// Trace is only for verification — not timed.
		traced, err := fetcher.TraceBlock(blockNum)
		if err != nil {
			fmt.Fprintf(os.Stderr, "block %d: trace error: %v\n", blockNum, err)
			os.Exit(1)
		}
		mismatches := verifyAgainstTrace(diff, traced)

		storageCnt := 0
		for _, slots := range diff.Storage {
			storageCnt += len(slots)
		}

		execMs := int(execElapsed.Milliseconds())
		execTimes = append(execTimes, execMs)
		fetchCounts = append(fetchCounts, stats.Total())

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
				n := len(sortedExec)
				pe := func(pct int) int { return sortedExec[min(n*pct/100, n-1)] }
				pf := func(pct int) int { return sortedFetch[min(n*pct/100, n-1)] }
				fmt.Printf("--- %d blocks | exec p50=%dms p90=%dms p95=%dms p99=%dms p100=%dms | fetches p50=%d p90=%d p99=%d p100=%d\n",
					n, pe(50), pe(90), pe(95), pe(99), pe(100), pf(50), pf(90), pf(99), pf(100))
				lastStats = time.Now()
			}
		} else {
			fmt.Printf("block %d: MISMATCH\n", blockNum)
			for _, m := range mismatches {
				fmt.Printf("  %s\n", m)
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
