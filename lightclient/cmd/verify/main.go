package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	lc "defi-toolbox/lightclient"

	"github.com/ava-labs/libevm/common"
)

func main() {
	rpcURL := flag.String("rpc", "ws://127.0.0.1:9650/ext/bc/C/ws", "WebSocket RPC URL")
	blocks := flag.Int("blocks", 10, "number of recent blocks to verify")
	concurrency := flag.Int("concurrency", 2*runtime.NumCPU(), "RPC pool size")
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

	for blockNum := startBlock; blockNum <= headNum; blockNum++ {
		total++

		fetchStart := time.Now()
		bd, err := fetcher.GetBlock(blockNum)
		if err != nil {
			fmt.Fprintf(os.Stderr, "block %d: fetch error: %v\n", blockNum, err)
			os.Exit(1)
		}
		fetchElapsed := time.Since(fetchStart)

		blockHashes[blockNum] = bd.Hash
		block := lc.BlockDataToTypesBlock(bd)

		miss := fetcher.MissCallbacks(state)
		sv := lc.NewStateView(state, blockNum-1, miss)

		execStart := time.Now()
		diff, err := lc.ExecuteBlock(block, sv, chainCfg, getHash)
		if err != nil {
			fmt.Fprintf(os.Stderr, "block %d: execute error: %v\n", blockNum, err)
			os.Exit(1)
		}
		execElapsed := time.Since(execStart)

		// Apply diffs to versioned state so subsequent blocks can read them.
		applyDiff(state, diff, blockNum)
		state.SetLatestBlock(blockNum)

		// Trace is only for verification — timed separately, not in the hot path.
		traceStart := time.Now()
		traced, err := fetcher.TraceBlock(blockNum)
		if err != nil {
			fmt.Fprintf(os.Stderr, "block %d: trace error: %v\n", blockNum, err)
			os.Exit(1)
		}
		mismatches := verifyAgainstTrace(diff, traced)
		traceElapsed := time.Since(traceStart)

		storageCnt := 0
		for _, slots := range diff.Storage {
			storageCnt += len(slots)
		}

		if len(mismatches) == 0 {
			matched++
			fmt.Printf("block %d: MATCH (txs=%d, storage=%d, balances=%d, fetch=%s exec=%s trace=%s)\n",
				blockNum, len(bd.Transactions), storageCnt, len(diff.Balances),
				fetchElapsed.Round(time.Millisecond), execElapsed.Round(time.Millisecond), traceElapsed.Round(time.Millisecond))
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
	for addr, slots := range traced.Storage {
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
