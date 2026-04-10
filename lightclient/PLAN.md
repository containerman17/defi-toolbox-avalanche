# Light Client — Implementation Plan

Working document. Updated as modules get implemented.

## Architecture Overview

```
┌─────────────────────────────────────────────┐
│                  LightClient                │
│  - Owns the block loop                      │
│  - Coordinates RPC pool, state, execution   │
│  - Exposes: LatestBlock(), Call()            │
├─────────────────┬───────────────────────────┤
│   Block Loop    │      EVM Executor         │
│  - Subscribe    │  - Takes resolved block#  │
│  - Fetch block  │  - Builds vm.BlockContext  │
│  - Execute      │  - Runs txs sequentially  │
│  - Update state │  - Returns diffs          │
├─────────────────┴───────────────────────────┤
│              Versioned State                 │
│  - Per-key linked list by block number      │
│  - Atomic "latest block" pointer            │
│  - Lock-free reads after block resolution   │
│  - vm.StateDB interface for EVM             │
├─────────────────────────────────────────────┤
│              RPC Pool                       │
│  - N blocking worker goroutines             │
│  - Shared work queue (channel)              │
│  - Natural backpressure from socket count   │
├─────────────────────────────────────────────┤
│              Snapshots                      │
│  - Serialize versioned state (latest only)  │
│  - Atomic write (tmp + rename)              │
│  - Load on startup                          │
└─────────────────────────────────────────────┘
```

## Modules

### Module 1: Versioned State (`state.go`)

The foundation. Everything else depends on this.

**Data structure:**
```
type versionedValue struct {
    block uint64
    value common.Hash       // for storage slots
    prev  *versionedValue
}
```

Each storage key (address + slot) maps to a linked list head. Accounts (balance, nonce, code) use separate maps with similar versioned chains.

**Operations:**
- `SetStorage(addr, slot, value, blockNum)` — prepend to list
- `GetStorage(addr, slot, blockNum)` — walk list, find entry with block <= requested
- `SetBalance(addr, balance, blockNum)` — same pattern
- `GetBalance(addr, blockNum)` — same pattern
- Same for nonce, code
- `LatestBlock() uint64` — atomic load
- `SetLatestBlock(n)` — atomic store
- `Prune(keepBlocks uint64)` — drop entries older than latest - keepBlocks

**vm.StateDB wrapper:**
A `StateView` struct that pins a block number at creation and implements `vm.StateDB`. All reads go through the versioned state at that pinned block. Writes go to a journal (for EVM execution — Snapshot/RevertToSnapshot support).

**Status:** Done (2026-04-10)

**Interfaces (consumed by other modules):**
```go
// StateStore is the versioned state storage.
type StateStore interface {
    GetStorage(addr common.Address, slot common.Hash, block uint64) common.Hash
    SetStorage(addr common.Address, slot common.Hash, value common.Hash, block uint64)
    GetBalance(addr common.Address, block uint64) *uint256.Int
    SetBalance(addr common.Address, balance *uint256.Int, block uint64)
    GetNonce(addr common.Address, block uint64) uint64
    SetNonce(addr common.Address, nonce uint64, block uint64)
    GetCode(addr common.Address, block uint64) []byte
    SetCode(addr common.Address, code []byte, block uint64)
    GetCodeHash(addr common.Address, block uint64) common.Hash
    LatestBlock() uint64
    SetLatestBlock(block uint64)
    Prune(keepBlocks uint64)
}
```

### Module 2: RPC Pool (`rpc.go`)

Blocking workers pattern.

**Design:**
- Pool of N WebSocket connections (default: 2 * NumCPU)
- Work queue: `chan rpcRequest`
- Each worker goroutine: loop { take request from queue, send to socket, block for response, deliver result }
- Caller sends request to queue, blocks on result channel
- Reconnect on disconnect (with backoff)

**Interface:**
```go
type RPCPool interface {
    Call(method string, params interface{}) (json.RawMessage, error)
    Close()
}
```

**Status:** Done

### Module 3: Block Fetcher (`fetch.go`)

Knows how to get block data from the node.

**Responsibilities:**
- Subscribe to new block headers via `eth_subscribe("newHeads")`
- Fetch full block by number via `eth_getBlockByNumber`
- Fetch state on cache miss: `eth_getStorageAt`, `eth_getBalance`, `eth_getCode`, `eth_getTransactionCount`
- Fetch block hash for BLOCKHASH opcode: `eth_getBlockByNumber` (header only)

**Interface:**
```go
type BlockFetcher interface {
    SubscribeNewHeads(ctx context.Context) (<-chan uint64, error)
    GetBlock(blockNum uint64) (*types.Block, error)
    GetStorageAt(addr common.Address, slot common.Hash, block uint64) (common.Hash, error)
    GetBalance(addr common.Address, block uint64) (*uint256.Int, error)
    GetCode(addr common.Address, block uint64) ([]byte, error)
    GetNonce(addr common.Address, block uint64) (uint64, error)
    GetBlockHash(blockNum uint64) (common.Hash, error)
}
```

Uses the RPCPool internally. This is the only module that talks to the network.

**Status:** Done (2026-04-10)

### Module 4: EVM Executor (`executor.go`)

Executes blocks and individual calls against the versioned state.

**Block execution:** Takes a full block + a StateView pinned at (block-1). Runs all transactions sequentially. Collects storage/balance/nonce/code diffs. Returns the diffs to the block loop which applies them to the versioned state at the new block number.

**User calls:** Takes a call message + block number. Creates a StateView at that block. Executes the call. Returns the result. Does not modify versioned state.

**Avalanche-specific:**
- Builds BlockContext with Avalanche header extensions (TimeMilliseconds, MinDelayExcess)
- Registers coreth params extras and custom types
- Atomic transactions from block Extra field (DEFERRED — noted in DESIGN.md)

**Interface:**
```go
type Executor interface {
    ExecuteBlock(block *types.Block, state vm.StateDB) (*BlockDiff, error)
    Call(msg CallMsg, state vm.StateDB, header *types.Header) ([]byte, uint64, error)
}
```

**Status:** Done (2026-04-10)

### Module 5: Snapshots (`snapshot.go`)

Persist and restore the versioned state.

**Format:** gob-encoded. Only stores the latest value per key (history is transient).

**Operations:**
- `Save(state StateStore, path string)` — serialize latest values, atomic write
- `Load(path string) (entries, error)` — deserialize, caller applies to state

**Status:** Done (2026-04-10)

### Module 6: LightClient (`client.go`)

The top-level coordinator. Only integration code.

**Startup:**
1. Create RPC pool
2. Create versioned state
3. Load snapshot if available
4. Create block fetcher
5. Catch up from snapshot block to current head
6. Subscribe to new blocks
7. Enter block loop

**Block loop:**
1. Receive new block number
2. Fetch full block
3. Create StateView at block-1
4. Execute block → get diffs
5. Apply diffs to versioned state at new block number
6. Advance latest block pointer (atomic)
7. Periodic snapshot save
8. Prune old history

**Public API:**
```go
func New(cfg Config) (*LightClient, error)
func (c *LightClient) Start(ctx context.Context) error
func (c *LightClient) LatestBlock() uint64
func (c *LightClient) Call(msg CallMsg, block uint64) ([]byte, uint64, error)
// block=0 means resolve latest atomically
```

**Cache miss flow:**
When EVM execution hits a storage slot not in the versioned state, the StateView calls back to the BlockFetcher to fetch it from the node. The fetched value is inserted into the versioned state at the block it was requested for (handles the "requested at block 5, arrived at block 7" case naturally).

**IMPORTANT — zero value storage:** The miss callback MUST store the fetched value into `VersionedState` even when it's zero. In partial state, absent ≠ zero. An absent key triggers an RPC fetch; a stored zero key is a cache hit. Without this, every read of a zero-valued slot hits RPC every time. The miss callbacks in client.go must: (1) fetch from RPC, (2) store into VersionedState at the requested block, (3) return the value.

**Status:** Not started

## Implementation Order

1. **Module 1 (Versioned State)** — no dependencies, foundation
2. **Module 2 (RPC Pool)** — no dependencies, standalone
3. **Module 3 (Block Fetcher)** — depends on RPC Pool
4. **Module 4 (EVM Executor)** — depends on Versioned State (vm.StateDB)
5. **Module 5 (Snapshots)** — depends on Versioned State
6. **Module 6 (LightClient)** — integrates everything

Modules 1 and 2 can be built in parallel. Module 4 can start once Module 1's interface is defined.

## Open Questions

- Exact pruning strategy: time-based? block-count-based? On every block or periodic?
- Snapshot format: plain gob or gob+zstd compression?
- How to handle reorgs (if at all — Avalanche has finality so maybe not needed)
- ~~GAS METERING~~ **FIXED (2026-04-10):** Root cause was `GetCommittedState` returning start-of-block state instead of start-of-transaction state. Added `CommitTx()` to snapshot the overlay between transactions. 500/500 blocks now match perfectly (storage + nonces vs trace).

## Implementation Log

_(agents update this section as they complete work)_

### Module 1: Versioned State — 2026-04-10

**File:** `lightclient/state.go`

**Implemented:**

1. **`VersionedState`** — core versioned store with per-key linked lists and atomic pointers for lock-free reads.
   - `sync.Map` for key-to-head lookup (one per data type: storage, balance, nonce, code).
   - Each linked list node is immutable after creation. `prev` pointers are `atomic.Pointer` so writers can prepend while readers walk concurrently.
   - One mutex per data type for writes. Writers hold the mutex and store new head.
   - `Prune(keepBlocks)` walks all chains and nils out prev pointers past the cutoff.
   - Code hashes precomputed at `SetCode` time, cached in `codeEntry`.

2. **`StateView`** — implements `vm.StateDB` pinned at a specific block number.
   - Reads: overlay -> versioned state -> miss callback (in that order).
   - Writes: map-based overlay with journal entries for snapshot/revert.
   - Journal-based `Snapshot()`/`RevertToSnapshot()` — O(mutations) not O(state).
   - `MissCallbacks` struct with per-type callbacks for RPC fetch on cache miss.
   - Dirty tracking maps (`DirtyStorage()`, `DirtyBalances()`, etc.) for diff extraction after block execution.
   - Avalanche-specific: `GetPredicate`, `GetBalanceMultiCoin`, `Add/SubBalanceMultiCoin`.
   - Minimal correct implementations for self-destruct, transient storage, access lists.

**Design decisions:**
- `sync.Map` instead of plain maps+RWMutex for key-to-head mapping. Avoids read locks on the hot path. Optimized for read-heavy workloads with stable keys.
- Separate mutex per data type rather than one global write lock. Eliminates contention between concurrent writes to different data types.
- `GetStorage` returns `(value, bool)` at the VersionedState level so StateView can distinguish "exists with zero value" from "not in store" (cache miss).
- Dirty tracking is separate from the overlay — reverts undo the overlay but dirty maps accumulate all writes. After block execution we want final values, not revert history.

### Module 2: RPC Pool — 2026-04-10

**File:** `lightclient/rpc.go`

**Implemented:**
- `RPCPool` struct with `NewRPCPool(url, size)`, `Call(method, params)`, `Close()`
- `SubscribeNewHeads(ctx)` returning `<-chan json.RawMessage`
- Blocking-workers pattern: N goroutines each own one WebSocket, pull work from a shared channel
- Each worker sends one JSON-RPC request, blocks for response, delivers result, then picks next work item
- Natural backpressure: when all workers are busy, callers block on the work queue

**Design decisions:**
- Work queue is buffered at pool size to avoid unnecessary blocking when workers are available but haven't pulled yet
- Connection errors trigger reconnect with exponential backoff (100ms to 5s) and automatic retry of the failed request
- RPC-level errors (e.g. invalid method, execution reverted) are returned to the caller without reconnecting
- `SubscribeNewHeads` uses a dedicated socket outside the worker pool; reconnects and resubscribes automatically on failure
- Subscription channel has capacity 16; notifications are dropped (not blocking) if the consumer is too slow
- Request IDs are pool-global atomically incrementing int64 (no need for per-socket IDs since each socket handles one request at a time, but unique IDs help with debugging)
- `Close()` is idempotent and waits for all workers to exit

### Module 4: EVM Executor — 2026-04-10

**File:** `lightclient/executor.go`

**Implemented:**

1. **`BlockDiff`** — struct capturing all state changes from a block: Storage, Balances, Nonces, Code.
2. **`CallMsg`** — parameters for simulated calls (eth_call equivalent).
3. **`GetHashFunc`** — type alias for `vm.GetHashFunc`, callback for BLOCKHASH opcode.
4. **`ExecuteBlock(block, state, chainCfg, getHash)`** — executes all transactions in a block sequentially against a StateView. Builds BlockContext with Avalanche-specific header handling (difficulty/random swap post-Shanghai). Uses `corethcore.ApplyMessage` for each transaction. Returns BlockDiff extracted from StateView's dirty tracking.
5. **`Call(msg, state, header, chainCfg, getHash)`** — executes a single simulated call without modifying underlying versioned state. Sets `SkipAccountChecks: true` for caller flexibility. Defaults to 50M gas cap. Returns raw return data, gas used, and error.
6. **`init()`** — registers `cparams.RegisterExtras()` and `ccustomtypes.Register()` once at package load.

**Design decisions:**
- Functions, not methods on a struct. No executor state needed — all context passed as parameters. The caller (LightClient) owns the chain config and getHash callback.
- `GetHashFunc` is a type alias (`=`) for `vm.GetHashFunc` rather than a new type, avoiding conversion friction.
- `ExecuteBlock` does not call `state.Prepare()` explicitly — `ApplyMessage` calls it internally via the state transition.
- Reverted transactions are not treated as errors in `ExecuteBlock`. They still modify nonces and consume gas; the dirty tracker captures the final state.
- `Call` uses `SkipAccountChecks: true` so callers don't need the sender to have sufficient balance/nonce for simulation.
- Atomic transactions (cross-chain imports/exports from block Extra) are deferred per DESIGN.md — the reference executor has them working but they require snow.Context setup.

### Module 3: Block Fetcher — 2026-04-10

**File:** `lightclient/fetch.go`

**Implemented:**

1. **`BlockFetcher`** — wraps `RPCPool` for all network interactions.
   - `GetBlock(blockNum)` — fetches full block via `eth_getBlockByNumber(hex, true)`, returns `*BlockData` with all header fields and parsed transactions.
   - `GetBlockHash(blockNum)` — fetches header-only via `eth_getBlockByNumber(hex, false)`, returns just the hash (for BLOCKHASH opcode).
   - `GetStorageAt(addr, slot, block)` — cache miss callback for storage slots.
   - `GetBalance(addr, block)` — cache miss callback for balances (returns `*uint256.Int`).
   - `GetCode(addr, block)` — cache miss callback for contract code.
   - `GetNonce(addr, block)` — cache miss callback for transaction counts.
   - `TraceBlock(blockNum)` — calls `debug_traceBlockByNumber` with prestateTracer diffMode, aggregates per-tx diffs into a single `TraceDiff`.
   - `MissCallbacks(state)` — returns a `MissCallbacks` struct wired to this fetcher; each callback fetches from node, stores in `VersionedState`, and returns the value.

2. **`BlockData`** — parsed block struct with standard Ethereum fields plus Avalanche extensions (`TimestampMilliseconds`, `MinDelayExcess`), raw `Extra` bytes for atomic transactions, and full transaction list.

3. **`TraceDiff`** — aggregated state diff from prestate tracer with exported maps for storage, balance, nonce, and code changes (uses `*uint256.Int` for balances instead of `common.Hash`).

**Design decisions:**
- Transaction parsing uses `types.Transaction` JSON unmarshaling (same approach as thin_client.go) — the libevm types handle all tx type variants natively.
- `TraceDiff` uses `*uint256.Int` for balance values (not `common.Hash` like thin_client.go) for type safety and easier integration with the versioned state which also uses `*uint256.Int`.
- `MissCallbacks` automatically populates the `VersionedState` on fetch, so subsequent reads for the same key at the same block hit the cache.
- Block parsing shared between `GetBlock` (full=true) and could be reused for header-only, but `GetBlockHash` uses a minimal struct to avoid unnecessary parsing.
- Null transactions in the block's tx array are skipped (same edge case handling as thin_client.go).

### Module 5: Snapshots — 2026-04-10

**File:** `lightclient/snapshot.go`

**Implemented:**

1. **Snapshot types** — `Snapshot`, `SnapshotStorageEntry`, `SnapshotBalanceEntry`, `SnapshotNonceEntry`, `SnapshotCodeEntry`. Balances stored as `[32]byte` big-endian via `uint256.Int.Bytes32()`.
2. **`SaveSnapshot(state, path)`** — iterates all keys via `ForEach*` methods, collects latest values into a `Snapshot` struct, gob-encodes to a temp file, fsyncs, and renames atomically.
3. **`LoadSnapshot(path)`** — opens file, gob-decodes into `*Snapshot`, returns it. Caller is responsible for applying.
4. **`ApplySnapshot(state, snap)`** — walks all snapshot entries and calls `SetStorage`/`SetBalance`/`SetNonce`/`SetCode` at the snapshot's block number, then sets `LatestBlock`.

**Also added to `state.go`:**
- `ForEachStorage(fn)`, `ForEachBalance(fn)`, `ForEachNonce(fn)`, `ForEachCode(fn)` — iteration methods that walk each `sync.Map` and read the head of each linked list. Used by `SaveSnapshot` to collect latest values.

**Design decisions:**
- Only the latest (head) value per key is serialized. History is transient and rebuilt from the chain on replay.
- Gob encoding chosen for simplicity and Go-native round-tripping. No external dependencies.
- Atomic write pattern (tmp + fsync + rename) prevents corrupted snapshots on crash.
- `ApplySnapshot` is a separate function (not part of `LoadSnapshot`) to keep loading pure and give the caller control over when state is mutated.
- Balance serialization uses fixed `[32]byte` big-endian format to avoid gob's `*big.Int` encoding overhead and ensure deterministic round-tripping through `uint256.Int`.
