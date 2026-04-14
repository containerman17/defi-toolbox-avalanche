# lightclient

Avalanche C-Chain light client. Syncs state from a live node via WebSocket, executes blocks locally, provides EVM simulation against current state.

Not a generic EVM client. Follows Avalanche upgrade semantics, handles atomic transactions (cross-chain imports/exports), warp precompile, and platform-level state changes.

## Usage

```go
package main

import (
    "context"
    "fmt"
    "log"

    "lightclient"
)

func main() {
    client, err := lightclient.New(lightclient.Config{
        DataDir: "./data",
        // RPCURL defaults to ws://127.0.0.1:9650/ext/bc/C/ws
        // FixedBlock defaults to 0 (live-following mode)
        // Concurrency defaults to 2 * NumCPU
    })
    if err != nil {
        log.Fatal(err)
    }

    // Option 1: run the block loop (blocks until context cancelled)
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    go client.Start(ctx)

    // Option 2: get the latest block and simulate a call
    block := client.LatestBlock()
    ret, gas, err := client.Call(lightclient.CallMsg{
        From: common.HexToAddress("0x..."),
        To:   &contractAddr,
        Data: calldata,
    }, block) // block=0 resolves to latest atomically
    fmt.Printf("result=%x gas=%d\n", ret, gas)
}
```

To pin the client to a single block instead of following live head:

```go
client, err := lightclient.New(lightclient.Config{
    DataDir:    "./data",
    FixedBlock: 82067033,
})
if err != nil {
    log.Fatal(err)
}
if err := client.Start(context.Background()); err != nil {
    log.Fatal(err)
}
defer client.Close()
// Start returns immediately in fixed mode. State fills lazily during calls.
```

In fixed-block mode, snapshots are stored under `DataDir/<block>.snapshot`.
Live mode continues to use `DataDir/state.snapshot`.

## Architecture

```
LightClient
  -> RPCPool (N blocking WebSocket workers, natural backpressure)
  -> BlockFetcher (block data, state cache-miss fetching, trace verification)
  -> VersionedState (per-key linked list, lock-free reads, atomic block resolution)
  -> EVM Executor (block execution with prefetch, atomic txs, user calls)
  -> Snapshots (gob-encoded, atomic write, load on startup)
```

### Block processing pipeline

1. Receive new block notification via WebSocket subscription
2. Fetch full block (txs included) — ~2ms
3. Prefetch: fire all txs in parallel on separate StateViews to warm cache
4. Execute block sequentially on the real StateView — p50=8ms, p90=35ms
5. Apply state diffs to versioned state
6. Advance latest block pointer (atomic)

### State model

Each storage key maps to a linked list of (value, blockNumber) entries, newest first. Reads walk the list to find the entry at or before the requested block. Writers prepend new entries. "Latest" resolves to a concrete block number atomically at the start of an operation — no split-brain reads.

Cache misses trigger RPC fetches that populate the versioned state. Zero values are stored explicitly (absent != zero in partial state).

## Performance

Measured on 16-core machine with local Avalanche node, 3800+ blocks verified:

| Metric | Value |
|--------|-------|
| exec p50 | 8ms |
| exec p90 | 35ms |
| exec p95 | 72ms |
| exec p99 | 516ms |
| block fetch | ~2ms |
| cache miss fetches p50 | 33/block (drops as cache warms) |

The p99 outliers are blocks with many new contracts/pools that trigger cache misses. Prefetching (parallel approximate execution) reduces p90 by 3-4x vs serial execution.

## Commands

### verify

Executes recent blocks locally and compares storage/nonce diffs against `debug_traceBlockByNumber`. Prints running exec time percentiles and cache miss stats every 5 seconds.

```bash
go run ./lightclient/cmd/verify/ --blocks 5000
go run ./lightclient/cmd/verify/ --blocks 5000 --rpc ws://node:9650/ext/bc/C/ws
```

### snapshot-bench

Tests the snapshot save/load pipeline. First run builds state from live blocks and saves a snapshot. Second run loads it and verifies execution is under 100ms.

```bash
go run ./lightclient/cmd/snapshot-bench/ --data-dir ./bench-data
# run again:
go run ./lightclient/cmd/snapshot-bench/ --data-dir ./bench-data
```

### debug-gas

Per-transaction gas comparison between local execution and chain receipts. Useful for isolating gas metering issues.

```bash
go run ./lightclient/cmd/debug-gas/ --block 82587541
```

## Avalanche-specific handling

- **Atomic transactions**: cross-chain imports/exports parsed from block extra data, applied via `EVMStateTransfer`
- **Snow context**: network ID, chain ID, AVAX asset ID wired into chain config for warp precompile
- **Header extensions**: `TimeMilliseconds`, `MinDelayExcess` (ACP-226) set on block headers
- **Platform state changes**: P-Chain staking rewards and atomic nonce changes handled via retry on `insufficient funds` / `nonce too high`
- **Upgrade tracking**: all Avalanche upgrades (Apricot phases through Helicon) loaded from `eth_getChainConfig`

## Files

| File | Lines | Purpose |
|------|-------|---------|
| state.go | ~1050 | Versioned linked-list state, vm.StateDB (StateView) |
| rpc.go | ~400 | Blocking-workers WebSocket RPC pool |
| fetch.go | ~510 | Block/state fetching, trace diffing, miss callbacks |
| executor.go | ~250 | Block execution with prefetch, user calls, atomic txs |
| snapshot.go | ~170 | Snapshot save/load/apply |
| client.go | ~480 | Top-level coordinator |
