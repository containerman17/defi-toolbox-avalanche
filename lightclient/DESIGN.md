# Light Client — Design

Isolated package. Syncs Avalanche C-Chain state from a live node, executes transactions locally. No DeFi/routing/formula knowledge — just state and execution.

**This is not a generic EVM light client.** It is Avalanche-specific. It follows Avalanche version upgrades and semantics, including C-Chain-only features like atomic transactions (cross-chain imports/exports via block extra data), Avalanche-specific header extensions, and the coreth rule set. It will track avalanchego/coreth releases.

Planned to move to its own repository later. Shares go.mod for now.

## What it does

1. Connects to a node via WebSocket
2. Fetches state on demand (storage slots, accounts)
3. Re-executes each block locally to stay current
4. Persists state to disk as atomic snapshots
5. Exposes EVM execution against the current (or recent) state

## Configuration

Minimal. Caller creates an instance with a struct:

- **RPC URL** — WebSocket endpoint of the node. Default: local node.
- **Data directory** — where snapshots are stored. Required.
- **Concurrency** — how many RPC sockets to open. Default: 2x CPU cores. (2x showed measurable improvement over 1x in testing; beyond that, gains are marginal.)
- **Start block** — omit or zero means follow live.

Everything else is internal.

## State storage — versioned linked list

Each storage key maps to a linked list of values ordered by block number:

```
key -> (value_block7) -> (value_block3) -> (value_block1) -> nil
```

Each entry: value + block number + pointer to previous entry.

### Block resolution

"Latest" atomically resolves to a concrete block number once, at the start of an operation. All state reads within that operation use that resolved block number. No split-brain — you never end up reading some keys from block N and others from block N+1.

### Reading

Walk the list from the head until finding an entry with block <= resolved block number. Hot path (latest block): one comparison, done.

### Writing (block update)

When a new block's diffs arrive, prepend new entries for changed keys. Never mutate existing entries. Atomically advance the "latest block" pointer.

This is append-only: readers that resolved to block N keep seeing consistent block-N state even while block N+1 is being applied. No read locks needed during writes.

### Late-arriving cache misses

When a storage slot is fetched via RPC (cache miss), the response may arrive blocks later than when it was requested. Example: requested at block 5, RPC responds at block 7.

This is fine. Insert the entry at block 5 in the list. If blocks 6 or 7 already wrote newer values, those sit ahead in the list. If they didn't touch this key, the block-5 value is correct for all subsequent blocks until overwritten. The linked list handles late insertions naturally — they slot into the right position by block number.

### Memory and pruning

Current full state is ~180 MB. Estimated growth: ~1 MB/s worst case (10k keys/block, hundreds of bytes each — both overestimates).

History of 20 blocks is plenty for in-flight reads to complete. Pruning: when prepending, if the tail is older than N blocks, drop it. Simple, bounded memory.

## Snapshots

Persisted to the data directory. Written atomically via temp file + rename (no partial writes). On startup, load the latest snapshot and catch up from there.

A snapshot is the full state at one block: all known storage slots and account data.

## Locking strategy

Phase 1 (now): RWMutex. Writers lock during block application, readers take read locks. Simple, correct.

Phase 2 (later): Lock-free reads. The append-only linked list design makes this possible — writers prepend, readers walk, no contention. The "latest block" pointer is swapped atomically. This is the target architecture but not required for initial implementation.

## Interface

As close to the EVM internals as possible. No convenience wrappers, no marshaling layers. If the caller has a `vm.Message`, they pass it directly. The package exposes what the EVM needs to execute, nothing more.

Returns: execution result + the block number it executed against.

## RPC socket model

Blocking workers pattern, not round-robin. Each socket is a goroutine that picks work from a shared queue, makes one RPC call, blocks until the response arrives, then picks the next item. With 48 sockets on a 24-core machine, exactly 48 requests can be in flight simultaneously.

This creates natural backpressure. If the node is slow, workers block, the queue fills up, and callers wait. No unbounded request fan-out, no need for rate limiting or semaphores — the socket count IS the concurrency limit.

## Avalanche-specific: atomic transactions

The block `Extra` field contains cross-chain atomic transactions (imports/exports between X-Chain and C-Chain). These are not EVM transactions — they directly credit/debit AVAX balances on accounts via `EVMStateTransfer()`. The light client must process these during block execution to maintain correct state.

Implementation exists in `/home/ubuntu/experiments/2026-04/01_block_fetcher/executor/executor.go` (in progress — one block still producing a wrong state root). Once that's resolved, the atomic tx handling will be ported here.

Dependencies: `coreth/plugin/evm/atomic` package, `snow.Context` with AVAX asset ID.

## Non-goals

- No DeFi, pool, formula, or routing knowledge
- No HTTP/REST API (callers import as a Go library)
- No multi-chain support (Avalanche C-Chain only)
- No transaction submission (read-only execution)
