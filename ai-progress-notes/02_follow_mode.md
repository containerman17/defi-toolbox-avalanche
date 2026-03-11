# Follow Mode (Block Following)

## Two Modes

The Rust quoter operates in two modes:

### Pinned Mode (current)
- CLI: `cargo run --release -- <router_address> <block_number>`
- Fetches state on-demand via RPC for the given block
- Stateless — no persistent snapshot, every slot read is an RPC call (cached per session)
- Used by example 03 for deterministic gas comparison

### Follow Mode (new)
- CLI: `cargo run --release -- <router_address> --follow`
- Long-lived process that tracks the chain head
- Maintains a persistent `StateSnapshot` updated by block diffs
- Used for BFS routing where we need fresh state continuously

## Architecture

Two threads:

### Block Follower Thread (background)
1. Poll `eth_blockNumber` every 200ms
2. When new block(s) appear, call `debug_traceBlockByNumber` with `prestateTracer` in diff mode for each new block
3. Trace calls can be parallelized (multiple blocks behind), but diffs must be applied in sequential block order
4. Push `(block_num, block_timestamp, basefee, storage_diffs)` into a channel

### Main Thread (request processing)
Loop:
1. Drain all pending block diffs from the channel, apply them to the snapshot sequentially
2. Process incoming ndjson requests (quote, bfs, etc.) against the current snapshot
3. **Never apply diffs while processing requests** — snapshot is immutable during computation
4. After processing, drain diffs again before next iteration

## StateSnapshot

```
StateSnapshot {
    block_num: u64,
    block_timestamp: u64,
    basefee: u64,
    storage: HashMap<Address, HashMap<B256, B256>>,
}
```

### apply_diffs(block_num, timestamp, basefee, diffs) -> Self
- Consumes old snapshot, returns new one
- Asserts `block_num == self.block_num + 1` (sequential)
- Merges storage diffs: for each (address, slots), overwrite existing slot values
- Updates block metadata

### Initial State
- On startup, the snapshot is empty (`block_num = latest - 1`)
- First RPC calls populate the snapshot via cache misses (cold path)
- Subsequent blocks apply diffs on top — only changed slots are updated
- Slots not in any diff retain their values from the initial RPC fetch

## State Read Hierarchy (SnapDB)

When the EVM reads a storage slot during a quote:

1. **Per-query overrides** — balance/allowance overrides for simulation (e.g., giving the router token balance)
2. **Snapshot** — the canonical chain state built from initial fetch + applied diffs
3. **Shared cache** — slots fetched via RPC during this block's computation, shared across threads
4. **RPC fallback** — cold miss, fetch from node via `eth_getStorageAt` at current block

In pinned mode, layers 1 and 4 are the only ones that matter (no snapshot, no shared cache).
In follow mode, layer 2 (snapshot) handles most reads after warmup — RPC fallback only for slots never previously touched.

## Diff Format (from debug_traceBlockByNumber)

The prestateTracer in diff mode returns pre/post state for every address touched by each transaction in a block. We only care about the `post` state — specifically changed storage slots:

```json
{
  "0xaddr...": {
    "storage": {
      "0xslot...": "0xvalue..."
    }
  }
}
```

We flatten all transactions in a block into a single `HashMap<Address, HashMap<Slot, Value>>`, later transactions overwriting earlier ones (correct — last write in a block wins).

## Why debug_traceBlockByNumber and not eth_getStorageAt

Polling individual slots would require knowing which slots changed. We don't know that — pools have complex internal state (reserves, ticks, liquidity positions). Tracing the entire block gives us ALL storage changes in one call, regardless of which contracts or slots were touched. This is O(1) RPC calls per block instead of O(n) where n is the number of active pools.

A non-archival RPC with debug namespace enabled is sufficient — we only trace the latest blocks, never historical ones.
