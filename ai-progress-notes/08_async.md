# Async Quote Processing

## Problem

Each `quote` op runs a 4-layer BFS that achieves only 2-3x parallelism out of 16 threads due to sequential layer barriers. 13 threads sit idle during each quote. Running multiple quotes concurrently would saturate the rayon pool.

## Design

### Protocol

No API change. The existing `quote` op with `id`-based request/response matching already supports concurrent requests. TypeScript can fire multiple `send()` calls without awaiting:

```typescript
const promises = quotes.map(q => send({ op: "quote", ...q }));
const results = await Promise.all(promises);
```

### Rust: Concurrent quote dispatch

The main stdin loop spawns `handle_quote` on a thread instead of running it synchronously. Responses are written back with the matching `id`. Mutating ops (`set_pools`, `prepare`, `set_block`) remain synchronous — they block the main loop and wait for in-flight quotes to drain.

### Shared state

| State | Access pattern | Synchronization |
|-------|---------------|-----------------|
| `prepared` | Read during quote, written by `prepare` | `Arc<RwLock>` |
| `pools_by_token` | Read during quote, written by `set_pools` | `Arc<RwLock>` |
| `phase2_cache` | Read+insert during quote, cleared by `prepare` | `Arc<RwLock>` |
| `snapshot` | Read during quote, merge fetched slots after BFS layers | `Arc<RwLock>` |
| `stdout` | Write from any thread | `Arc<Mutex>` |

### Mutating ops safety

`set_pools`, `prepare`, and `set_block` must not run concurrently with quotes. The main loop tracks in-flight quote count via `Arc<AtomicUsize>`. Before a mutating op, it waits for in-flight count to reach 0 (no new quotes are dispatched while waiting since the main loop is single-threaded).

### Snapshot merging

Each BFS layer merges fetched slots into the snapshot between layers. With concurrent quotes, two quotes may fetch overlapping slots. `Arc<RwLock<StateSnapshot>>` handles this — each layer takes a write lock briefly to merge. This is safe because merging is idempotent (same slot = same value for a given block).

## Results

### Benchmark: 8 hot quotes, 7500 pools, max_pools=500, 16 threads

| Mode | Wall time | EVM calls | Throughput |
|------|-----------|-----------|------------|
| Sequential | 1215ms | 9776 | 8,048 calls/sec |
| Parallel (8 concurrent) | 655ms | 9776 | 14,923 calls/sec |
| **Speedup** | **1.9x** | — | **1.9x** |

### Analysis

- Individual quote BFS time increases when running concurrently (150ms solo → 430-650ms with 8 concurrent) due to rayon pool contention
- Total throughput nearly doubles: idle threads during layer barriers are now filled by other quotes' work
- Per-core EVM throughput is ~940 calls/sec — this is the hardware limit
- Theoretical max at 16 threads: ~15K calls/sec — we're hitting that ceiling
- On 24-core hardware, parallel quotes would scale further (more idle threads to fill)

### Why only 1.9x and not 8x?

The rayon thread pool is shared. When 8 quotes run concurrently, they all submit work to the same 16 rayon threads. The threads are now fully saturated (good!), but there are only 16 of them. Total compute is the same — we just eliminated idle time during layer barriers.

The 1.9x comes from: sequential had ~50% thread utilization (2-3x of 16), parallel has ~95% utilization. 95/50 ≈ 1.9x.

### What didn't work / what we learned

1. **H1-H3 from speed investigation (bytecode clone, SnapDB alloc, JUMPDEST)** — all disproven. Per-call overhead is ~12us (setup 5-6us + build 3-7us), negligible vs ~400us exec time. `revm::Bytecode` is `Arc<LegacyAnalyzedBytecode>` — clone is atomic refcount bump, not memcpy.

2. **Terminal re-quote elimination** — can't do it. Pools share state (e.g. Uniswap V3 positions on same pair, shared oracles), so chained single-hop amounts diverge from full-path simulation. Must keep full-path re-quotes for correctness.

3. **Top-N terminal filtering** — can't do it. Terminal jobs go directly to full-path re-quote with no prior single-hop estimate to rank by.

4. **Concurrent quotes (this doc)** — works but modest gain (1.9x). We're now at the hardware throughput ceiling (~15K calls/sec on 16 threads).

### Remaining optimization options

- **Run intermediates + terminals concurrently within each layer** — they're independent. Saves 1 barrier per layer. For max_hops=4, saves 2 barriers (~60ms).
- **More cores** — the improvement scales linearly with available threads since we're compute-bound.
