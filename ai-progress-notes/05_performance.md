# Performance Analysis & Benchmarks

## Architecture

- **Transport**: 8 persistent WebSocket connections pooled via crossbeam-channel. Rust reads `WS_RPC_URL` env var (e.g. `ws://localhost:9650/ext/bc/C/ws`).
- **Allocator**: jemalloc (tikv-jemallocator) for reduced contention under parallel workloads.
- **Parallelism**: rayon thread pool. Phase 0+1 parallelize EVM calls. Phase 2 parallelizes across directions via `par_iter`.

## Benchmark: 7,500 pools, 1 size tier, hot prepare

Measured on a 24-vCPU VM (AMD EPYC 9645, but vCPUs share physical resources — see VM caveat below).

### Phase 0 (token pricing): 67ms
- 3,027 EVM calls, prices 2,353 tokens
- 2-wave BFS from WAVAX at 0.1 AVAX probe

### Phase 1 (rate table): 200ms
- 15,004 EVM calls (one per edge), rates 6,358 edges
- One `quote_all` batch per size tier (QuoteKey doesn't include amount, so tiers can't be batched together)
- Scales linearly with number of size tiers: N tiers × 15k calls

### Phase 2 (f64 path enumeration): parallel across directions

Per-direction timings (single-thread per direction):

| Direction | Time | Iterations | Pools selected |
|-----------|------|------------|----------------|
| USDC→WAVAX | 5.0ms | 499k | 847 |
| WAVAX→USDC | 7.3ms | 908k | 840 |
| USDC→USDC | 50ms | 7.9M | 849 |
| WAVAX→WAVAX | 27ms | 2.8M | 856 |
| WAVAX→BTC.b | 12.4ms | 2.8M | 50 |
| USDC→WETH.e | 35.3ms | 6.7M | 833 |

Roundtrip directions (A→A) are most expensive due to the combinatorial explosion through hub tokens.

### Phase 2 scaling (parallel, 7,500 pools, 1 size tier)

| Top N tokens | Directions | Phase 0+1 | Phase 2 CPU | Phase 2 wall | Hot total |
|---|---|---|---|---|---|
| 5 | 25 | 280ms | 624ms | 47ms | 344ms |
| 20 | 400 | 270ms | 5.6s | 263ms | 575ms |
| 50 | 2,500 | 270ms | 19s | 1.7s | 2.0s |
| 100 | 10,000 | 260ms | 41s | 7.6s | 8.0s |

### Direction time distribution (10,000 directions)

```
0-100μs:     6,002  (60% — token pairs with no paths)
100-500μs:       1
500-1000μs:      8
1-5ms:         806
5-10ms:      1,594
10-20ms:       998
20-50ms:       580
50-100ms:       11
```

Most directions are trivial (no reachable paths). The expensive tail is roundtrip pairs through hub tokens.

## VM Caveat: vCPU ≠ real cores

The benchmark VM has 24 vCPUs (AMD EPYC 9645) but they share physical resources. A pure sin() benchmark shows only 4.1x speedup on 24 vCPUs, confirming the hypervisor limits parallel throughput.

Evidence:
- `lscpu`: 24 sockets × 1 core/socket × 1 thread/core — this is a VM topology
- Pure rayon sin() benchmark: 116s single-threaded → 28s on 24 vCPUs = 4.1x (not 24x)
- Phase 2 reports avg_active=22 threads but /proc/stat shows 30% CPU utilization
- Phase 2: 41s CPU / 7.6s wall = 5.4x — matches the VM's ~4-5x ceiling

**On real hardware with 24 physical cores, expected Phase 2 wall times:**
- 10,000 directions: 41s CPU / 24 = ~1.7s wall
- Full hot prepare (100 tokens × 1 tier): ~270ms + 1.7s ≈ 2s

## Scaling considerations for production

### Phase 1 with multiple size tiers
- N tiers × 15k edges = N × 15k EVM calls
- 6 tiers: ~90k calls, ~1.2s hot on this VM
- On real hardware: ~120ms

### Phase 2 at scale
- 100 tokens = 10,000 pairs × M size tiers = 10,000×M directions
- Each direction: avg 4ms, max ~80ms
- Most of the cost is in roundtrip (A→A) directions and pairs through WAVAX

### Potential optimizations (not yet implemented)
- **Phase 2 within-direction parallelism**: Split first-hop edges across threads for heavy directions (50ms+). Would help the long tail.
- **Incremental Phase 1**: After a block diff, re-probe only affected edges instead of all 15k.
- **Phase 2 caching**: Many directions share sub-paths. Forward 2-hop arrivals from WAVAX are reused across all WAVAX→X directions — could precompute once.
- **Adjacency as flat arrays**: Replace HashMap with indexed Vec for better cache locality in Phase 2 enumeration.

## Roundtrip bug fix (2026-03-13)

Phase 2 had a bug where token revisit checks pruned valid roundtrip paths. For A→A directions, the check `if e2.token_out == token_in { continue }` was killing all 2-hop A→X→A paths before they could be scored. Fixed by skipping the token_in check when `token_in == token_out`. The terminal check (`if eN.token_out == token_out → score, stop`) naturally prevents the token from being used as a non-terminal intermediate.

WAVAX→WAVAX went from 1.3ms (incorrectly pruned, 123k iters) to 27ms (correct, 2.8M iters) at 7,500 pools.
