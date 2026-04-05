# Split Routing Strategies

Split routing divides a large swap into smaller chunks to reduce price impact. Each chunk runs BFS to find the best path on the current (depleted) pool state.

## Quick Start

```go
result := splitter.Split(params, amountIn)  // recommended default
```

`Split()` uses `GreedyCompete(30, 2)` — the only strategy that **provably never returns less than the single-path result.** 42 wins, 0 losses, ~540ms.

## Strategies

### `Split` (default) — GreedyCompete c30_2

**Use this unless you have a reason not to.** Processes volume in 30% slabs. For each slab, competes single-shot vs 2% chunks — whichever produces more output wins. This guarantees the result is always >= the single-path result.

```go
result := splitter.Split(params, amountIn)
// or directly:
result := splitter.GreedyCompete(params, amountIn, 30, 2)
```

- **Provably never loses** — min = +0.000% across all 57 test cases
- 42 wins vs Greedy, 0 losses, ~540ms
- When splitting helps: chunks win the tournament. When it doesn't: single wins automatically

**Why not other strategies?** `GreedyMixed(grad)` and `GreedyMixed(shuf2)` get 1 more win (43) but occasionally lose. GreedyCompete trades that 1 win for the guarantee. On volatile market conditions where mixed schedules pick up losses, GreedyCompete stays safe.

### `Greedy` — equal chunks, full BFS per chunk

The baseline everything is measured against. Divides volume into N equal chunks, runs BFS + EVM per chunk with dirty-slot overlay.

```go
result := splitter.Greedy(params, amountIn, 10)
```

- ~130ms for 10 chunks
- Solid but sometimes loses to strategies with more/smarter chunks

### `Optimized` — pre-discovered paths, formula selection

Discovers paths upfront (BFS at 5% and 100% volume), then selects per chunk via formula quotes. Fastest strategy but misses paths that only emerge after pool depletion.

```go
result := splitter.Optimized(params, amountIn, 10)
```

- ~60ms — fastest
- ~12 losses vs Greedy (misses diverse paths)

### `GreedyDynamic` — adaptive chunk sizing

Adapts chunk size based on whether BFS finds the same or different path vs previous chunk. Same path → small chunk. Different path → large discovery chunk.

```go
result := splitter.GreedyDynamic(params, amountIn, 8, 2)
```

- d8_2: ~300ms, 33-41 wins, 0-3 losses (varies by block state)
- Previously the default before GreedyCompete was discovered

### `GreedyCompete` — tournament split (the default)

For each slab, runs both single-shot AND chunked, picks the winner. See `Split` above.

```go
result := splitter.GreedyCompete(params, amountIn, 30, 2)  // 30% slabs, 2% chunks
result := splitter.GreedyCompete(params, amountIn, 50, 5)  // faster, fewer wins
```

### `GreedyRecursive` — binary split tournament

Recursively splits in half, competing single vs halves at each level. Sound in theory but slower than GreedyCompete due to re-execution overhead for dirty slot tracking.

```go
result := splitter.GreedyRecursive(params, amountIn, 2)  // min 2% chunks
```

- 40 wins, 0 losses, ~930ms — correct but too slow

### `GreedyFast` — incremental overlay

Greedy with incremental `PoolManagerOverlay` that reuses cached pool quoters across chunks.

```go
result := splitter.GreedyFast(params, amountIn, 40)
```

- ~400ms for 40 chunks
- Zero losses at 1x volume, can lose at very small volumes

### `GreedyMixed` — parameterized chunk schedules

Pre-defined schedules of decreasing chunk sizes.

```go
result := splitter.GreedyMixed(params, amountIn, splitter.SchedGradual)
result := splitter.GreedyMixed(params, amountIn, splitter.SchedShuffle2)
```

- Most wins (43) but occasionally loses (up to 5 losses)

### `GreedySplit` — decoupled discovery/execution

BFS at a large probe volume, execute at smaller volume. Proved the concept but GreedyCompete does it better.

### `Best` — run all, pick highest

Runs multiple strategies, returns the best. For when time doesn't matter.

```go
result := splitter.Best(params, amountIn, 10)
```

## Benchmark Summary

57 test cases: 17 directional pairs (WAVAX/USDC/USDT/sAVAX/WETH.e) × 3 volumes (1x/÷10/÷100) at ~$1M base.

| Strategy | Wins | Losses | Min | Median time | Guarantee |
|---|---|---|---|---|---|
| **GreedyCompete c30_2** | **42** | **0** | **+0.000%** | **540ms** | **never loses** |
| GreedyRecursive rec2 | 40 | 0 | +0.000% | 930ms | never loses |
| GreedyMixed grad | 43 | 0-5 | -0.11% | 460ms | no |
| GreedyMixed shuf2 | 43 | 0-5 | -0.04% | 310ms | no |
| GreedyDynamic d8_2 | 33-41 | 0-3 | -0.00% | 300ms | no |
| GreedyFast 40 | 42 | 0-2 | -0.00% | 400ms | no |
| Greedy 10 | — | — | — | 130ms | baseline |
| Optimized 10 | 0 | 12 | — | 60ms | no |

"Wins/Losses" are vs Greedy 10 (baseline). Ranges reflect variation across different block states.

## Key Findings

1. **More chunks always helps** — smaller chunks = less price impact per chunk
2. **BFS discovers different paths at different volumes** — topK=3 beam keeps different candidates depending on input amount
3. **Splitting the same path into chunks is always worse** (concavity) — split only helps when it finds different paths for different chunks
4. **Tournament competition (GreedyCompete) guarantees non-negative** — when splitting hurts, single-shot wins automatically
5. **LFJ V2 quote caching** gave a global 47% speedup across all strategies
6. **Incremental overlay** (`UpdateDirtySlots`) avoids rebuilding pool quoters from scratch each chunk

## Dead Ends

- **Water-filling** (marginal equilibrium binary search): paths share pools, can't allocate independently
- **Frank-Wolfe** (convex optimization): same shared-pool problem
- **Adaptive chunk sizing** (rate-based): large chunks over-deplete pools
- **Top-5 BFS per chunk**: same output as top-1
- **Reusing previous path**: BFS already considers it
- **Forced periodic re-discovery**: path-change signal is already sufficient
