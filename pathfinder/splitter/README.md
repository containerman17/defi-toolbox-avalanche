# Split Routing Strategies

Split routing divides a large swap into smaller chunks to reduce price impact. Each chunk runs BFS to find the best path on the current (depleted) pool state.

## Quick Start

```go
result := splitter.Split(params, amountIn)  // recommended default
```

## Strategies

### `Split` (default) — GreedyDynamic d8_2

**Use this unless you have a reason not to.** Adaptive chunk sizing: 8% discovery chunks when BFS finds a different path, 2% fine-tune chunks when the path is stable.

- **Zero losses** across 57 test cases (17 pairs × 3 volume levels)
- ~210ms median, ~45 wins vs Greedy
- Automatically handles small volumes (barely splits) and large volumes (aggressively splits)

### `Greedy` — equal chunks, full BFS per chunk

The baseline. Divides volume into N equal chunks, runs BFS + EVM per chunk with dirty-slot overlay.

```go
result := splitter.Greedy(params, amountIn, 10)  // 10 equal chunks
```

- ~150ms for 10 chunks
- Solid but sometimes loses to strategies with more/smarter chunks

### `Optimized` — pre-discovered paths, formula selection

Discovers paths upfront (BFS at 5% and 100% volume), then selects per chunk via formula quotes. Fastest strategy.

```go
result := splitter.Optimized(params, amountIn, 10)
```

- ~60ms — fastest
- Misses diverse paths that only emerge after pool depletion (~12 losses vs Greedy)

### `GreedyFast` — incremental overlay, more chunks

Greedy with 4× more chunks and an incremental `PoolManagerOverlay` that reuses cached pool quoters across chunks instead of rebuilding from scratch.

```go
result := splitter.GreedyFast(params, amountIn, 40)
```

- ~400ms for 40 chunks — same output as GreedyFine but faster
- Zero losses at 1x volume, can lose at very small volumes (÷100)

### `GreedyFine` — just more chunks

Greedy with 4× the requested chunks. Simple but effective.

```go
result := splitter.GreedyFine(params, amountIn, 10)  // actually runs 40 chunks
```

- ~500ms — slower than GreedyFast (no incremental overlay)
- Same output as GreedyFast

### `GreedyDynamic` — adaptive chunk sizing

Adapts chunk size based on whether BFS finds the same or different path vs previous chunk.

```go
result := splitter.GreedyDynamic(params, amountIn, 8, 2)   // 8% discovery, 2% fine-tune
result := splitter.GreedyDynamic(params, amountIn, 8, 1)   // 8% discovery, 1% fine-tune
```

- **Zero losses** — the path-change signal naturally prevents over-chunking
- d8_2: ~210ms, d8_1: ~260ms

### `GreedyMixed` — parameterized chunk schedules

Pre-defined schedules of decreasing chunk sizes. Large chunks discover paths at scale, small chunks fine-tune.

```go
result := splitter.GreedyMixed(params, amountIn, splitter.SchedGradual)  // smooth 6%→2% taper
result := splitter.GreedyMixed(params, amountIn, splitter.SchedShuffle2) // 10,5,2 repeating
```

Available schedules: `SchedFrontLoaded`, `SchedGradual`, `SchedPlateau`, `SchedMagic`, `SchedShuffle1-4`, `SchedBulk`.

- Most wins (41-48) but occasionally loses (2-5 losses)
- `SchedShuffle2`: safest mixed schedule (min loss -0.03%)
- `SchedGradual`: most wins

### `GreedySplit` — decoupled discovery/execution volume

BFS at a large probe volume to discover paths, execute at a smaller volume.

```go
result := splitter.GreedySplit(params, amountIn, 50, 2)  // probe at 50% remaining, exec at 2%
```

- Proved the concept (42 wins) but slower than mixed schedules for same benefit

### `Best` — run all, pick highest output

Runs Greedy, Optimized, GreedyFine, and Greedy 8× — returns the best result. For when compute time doesn't matter.

```go
result := splitter.Best(params, amountIn, 10)
```

## Benchmark Summary

57 test cases: 17 directional pairs (WAVAX/USDC/USDT/sAVAX/WETH.e) × 3 volumes (1x/÷10/÷100) at ~$1M base amounts.

| Strategy | Wins | Losses | Ties | Min loss | Median time |
|---|---|---|---|---|---|
| **GreedyDynamic d8_2** | **45** | **0** | **12** | **+0.00%** | **210ms** |
| GreedyDynamic d8_1 | 44 | 0 | 13 | +0.00% | 260ms |
| GreedyMixed grad | 48 | 2 | 7 | -0.09% | 440ms |
| GreedyMixed shuf2 | 46 | 4 | 7 | -0.04% | 300ms |
| GreedyFast 40 | 47 | 2 | 8 | -0.00% | 370ms |
| Greedy 10 | — | — | — | — | 110ms |
| Optimized 10 | 0 | 12 | 45 | — | 60ms |

"Wins/Losses" are vs Greedy 10 chunks (baseline).

## Key Findings

1. **More chunks always helps** — smaller chunks = less price impact per chunk
2. **BFS discovers different paths at different volumes** — the topK=3 beam keeps different candidates depending on input amount
3. **Mixed chunk sizes outperform equal chunks** — large discovery chunks + small fine-tune chunks get the best of both worlds
4. **Path-change signal enables zero-loss adaptive sizing** — bump to large chunk only when the best path changes
5. **LFJ V2 quote caching** (in `formulas/overlay.go`) gave a global 47% speedup across all strategies
6. **Incremental overlay** (`UpdateDirtySlots`) avoids rebuilding pool quoters from scratch each chunk
