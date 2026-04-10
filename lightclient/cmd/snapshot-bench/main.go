package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"defi-toolbox/lightclient"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

func main() {
	rpcURL := flag.String("rpc", "ws://127.0.0.1:9650/ext/bc/C/ws", "WebSocket RPC endpoint")
	dataDir := flag.String("data-dir", "./lightclient-bench-data", "directory for snapshot storage")
	concurrency := flag.Int("concurrency", 2*runtime.NumCPU(), "RPC pool size")
	blocks := flag.Int("blocks", 5, "number of blocks to process on first run")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "snapshot-bench: create data dir: %v\n", err)
		os.Exit(1)
	}

	snapPath := filepath.Join(*dataDir, "state.snapshot")

	// Check if snapshot already exists.
	if _, err := os.Stat(snapPath); err == nil {
		// --- Second run: load snapshot, benchmark a call ---
		runBenchmark(snapPath, *rpcURL, *concurrency)
	} else {
		// --- First run: build state from live blocks, save snapshot ---
		runFirstPass(snapPath, *rpcURL, *concurrency, *blocks)
	}
}

// wavaxAddr is the WAVAX contract on Avalanche C-Chain.
var wavaxAddr = common.HexToAddress("0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7")

// totalSupplySelector is the 4-byte selector for totalSupply().
var totalSupplySelector = common.Hex2Bytes("18160ddd")

func runFirstPass(snapPath, rpcURL string, concurrency, numBlocks int) {
	fmt.Println("snapshot-bench: no snapshot found, building state from live blocks...")

	pool, err := lightclient.NewRPCPool(rpcURL, concurrency)
	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot-bench: rpc pool: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	chainCfg, err := lightclient.FetchChainConfig(pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot-bench: chain config: %v\n", err)
		os.Exit(1)
	}

	state := lightclient.NewVersionedState()
	fetcher := lightclient.NewBlockFetcher(pool)

	// Get current head block.
	headNum, err := fetchHeadBlock(pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot-bench: fetch head: %v\n", err)
		os.Exit(1)
	}

	// Process N blocks ending at head.
	startBlock := headNum - uint64(numBlocks) + 1
	blockHashes := make(map[uint64]common.Hash)

	for blockNum := startBlock; blockNum <= headNum; blockNum++ {
		t := time.Now()

		bd, err := fetcher.GetBlock(blockNum)
		if err != nil {
			fmt.Fprintf(os.Stderr, "snapshot-bench: fetch block %d: %v\n", blockNum, err)
			os.Exit(1)
		}

		blockHashes[blockNum] = bd.Hash
		block := lightclient.BlockDataToTypesBlock(bd)

		miss := fetcher.MissCallbacks(state)
		sv := lightclient.NewStateView(state, blockNum-1, miss)

		diff, err := lightclient.ExecuteBlock(block, sv, chainCfg, func(n uint64) common.Hash {
			if h, ok := blockHashes[n]; ok {
				return h
			}
			h, _ := fetcher.GetBlockHash(n)
			blockHashes[n] = h
			return h
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "snapshot-bench: execute block %d: %v\n", blockNum, err)
			os.Exit(1)
		}

		applyDiff(state, diff, blockNum)
		state.SetLatestBlock(blockNum)

		fmt.Printf("snapshot-bench: processed block %d (txs=%d, elapsed=%v)\n",
			blockNum, len(bd.Transactions), time.Since(t).Round(time.Millisecond))
	}

	// Also execute a Call to WAVAX.totalSupply() to populate that contract's state.
	{
		miss := fetcher.MissCallbacks(state)
		sv := lightclient.NewStateView(state, headNum, miss)

		bd, err := fetcher.GetBlock(headNum)
		if err != nil {
			fmt.Fprintf(os.Stderr, "snapshot-bench: fetch header for call: %v\n", err)
			os.Exit(1)
		}
		header := lightclient.BlockDataToTypesBlock(bd).Header()

		to := wavaxAddr
		_, _, err = lightclient.Call(
			lightclient.CallMsg{To: &to, Data: totalSupplySelector},
			sv, header, chainCfg,
			func(n uint64) common.Hash {
				if h, ok := blockHashes[n]; ok {
					return h
				}
				h, _ := fetcher.GetBlockHash(n)
				blockHashes[n] = h
				return h
			},
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "snapshot-bench: warmup call failed: %v\n", err)
			os.Exit(1)
		}
	}

	// Count entries and save.
	storageCount, balanceCount := countState(state)

	if err := lightclient.SaveSnapshot(state, snapPath); err != nil {
		fmt.Fprintf(os.Stderr, "snapshot-bench: save snapshot: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("snapshot-bench: saved snapshot at block %d (storage=%d slots, balances=%d)\n",
		headNum, storageCount, balanceCount)
	fmt.Println("snapshot-bench: first run complete. Run again to benchmark from snapshot.")
}

func runBenchmark(snapPath, rpcURL string, concurrency int) {
	snap, err := lightclient.LoadSnapshot(snapPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot-bench: load snapshot: %v\n", err)
		os.Exit(1)
	}

	state := lightclient.NewVersionedState()
	lightclient.ApplySnapshot(state, snap)

	fmt.Printf("snapshot-bench: loaded snapshot at block %d (storage=%d slots, balances=%d)\n",
		snap.BlockNumber, len(snap.Storage), len(snap.Balances))

	// We need chain config for the Call. Connect briefly to fetch it.
	pool, err := lightclient.NewRPCPool(rpcURL, concurrency)
	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot-bench: rpc pool: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	chainCfg, err := lightclient.FetchChainConfig(pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot-bench: chain config: %v\n", err)
		os.Exit(1)
	}

	// Fetch the actual block header for the snapshot block.
	fetcher := lightclient.NewBlockFetcher(pool)
	bd, err := fetcher.GetBlock(snap.BlockNumber)
	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot-bench: fetch block %d: %v\n", snap.BlockNumber, err)
		os.Exit(1)
	}
	header := lightclient.BlockDataToTypesBlock(bd).Header()

	// No miss callbacks — everything must come from the snapshot.
	// If something is missing, it means the snapshot is incomplete, which is fine
	// for a simple totalSupply() call.
	noMiss := lightclient.MissCallbacks{}
	sv := lightclient.NewStateView(state, snap.BlockNumber, noMiss)

	fmt.Println("snapshot-bench: executing test call...")

	to := wavaxAddr
	start := time.Now()
	_, _, err = lightclient.Call(
		lightclient.CallMsg{To: &to, Data: totalSupplySelector},
		sv, header, chainCfg,
		func(n uint64) common.Hash { return common.Hash{} },
	)
	elapsed := time.Since(start)

	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot-bench: call failed: %v\n", err)
		os.Exit(1)
	}

	ms := elapsed.Milliseconds()
	fmt.Printf("snapshot-bench: call executed in %dms\n", ms)

	const threshold = 100
	if ms > threshold {
		fmt.Printf("snapshot-bench: FAIL (%dms > %dms threshold)\n", ms, threshold)
		os.Exit(1)
	}
	fmt.Printf("snapshot-bench: PASS (%dms < %dms threshold)\n", ms, threshold)
}

func applyDiff(state *lightclient.VersionedState, diff *lightclient.BlockDiff, block uint64) {
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

func countState(state *lightclient.VersionedState) (storageCount, balanceCount int) {
	state.ForEachStorage(func(_ common.Address, _ common.Hash, _ common.Hash) {
		storageCount++
	})
	state.ForEachBalance(func(_ common.Address, _ *uint256.Int) {
		balanceCount++
	})
	return
}

func fetchHeadBlock(pool *lightclient.RPCPool) (uint64, error) {
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
