# Arb4: Two-Phase BFS Arbitrage

## Context

This is a rewrite of the cyclic arbitrage scanner. Previous versions (arb1/arb2/arb3) are in
`experiments/arb1`, `experiments/arb2`, `experiments/arb3`. Arb1 is the current production
scanner. Arb4 replaces arb1's architecture with a fundamentally better approach.

### What arb1 does (the baseline)

Arb1 has a 4-phase pipeline that runs per block:

1. **Rate screening** — maintains float64 rate tables per pool (output/input ratio at 5 fixed
   size buckets: 0.001–10 AVAX). For each dirty cycle, multiplies rates across hops. If
   product > 0.99, it's a candidate. Top 500 candidates kept.
2. **Formula quoting** — calls `PoolManager.Quote()` sequentially for top 500 candidates.
   Filters to ~50 formula-profitable cycles.
3. **EVM verification** — runs top 50 through local EVM simulation via `statedb.CachedContext`.
   Calculates `EVMProfit = output - input - gasCost`.
4. **Execution** — signs and broadcasts via `HayabusaRouter.swap()`.

### What's wrong with arb1

- **Fixed size buckets (5 amounts)** — extremely coarse. Leaves profit on the table because the
  optimal amount is almost never one of the 5 buckets.
- **Rate table screening** — has ~7000 false positives per block (arb3 analysis). Float64 rate
  composition accumulates error across hops.
- **No gas awareness until Phase 3** — BFS/formula phase optimizes for gross output, not profit.
  A 4-hop cycle with slightly higher output but 2× gas can beat a cheaper 2-hop cycle in
  screening but lose after gas deduction.
- **WAVAX hub only** — misses cross-hub arb opportunities through USDC, USDT, WETH.e.
- **Cycle enumeration at startup** — enumerates all possible cycles upfront. Doesn't adapt to
  which pools actually have liquidity shifts.

## Arb4 Architecture: Two-Phase BFS

### Key insight

Formulas are precise (wei-level accuracy, 98%+ match rate with EVM). Any cycle where
`formula_output > input` is a real opportunity. The formula BFS is ground truth for amounts —
we only need EVM for gas measurement.

### Phase 1: Exclusion BFS (formula, broad discovery)

**Goal**: Discover a diverse set of pools that participate in profitable cycles.

**Algorithm**:
1. Run formula BFS in cyclic mode: `FindTopRoutes(pm, adj, pools, state, cfg, router, sender,
   HUB, HUB, probeAmount, maxHops, 1)` — finds the single best cycle.
2. Extract the pools from that cycle (e.g., [A, B, C] for a 3-hop).
3. **Fork**: Run BFS again N times, each time with one pool excluded from the adjacency graph:
   - Exclude A → find best cycle → gets [D, B, E]
   - Exclude B → find best cycle → gets [A, F, C]
   - Exclude C → find best cycle → gets [A, B, G]
4. Deduplicate by path identity. For each NEW path found, exclude its NEW pools (not yet in set)
   in another round.
5. Stop after 2 rounds or when new paths have `formula_output < input` (unprofitable).
6. Repeat at 3–5 different probe amounts (e.g., 0.1, 1, 10, 100 AVAX) to catch
   amount-sensitive paths.

**Output**: A set of 20–40 unique pools that participate in potentially profitable cycles.

**Why exclusion instead of topK**: TopK finds paths ranked by the same BFS beam — they share
most pools (path #2 is often path #1 with one hop swapped). Exclusion forces genuinely
different routes through different parts of the graph, producing real diversity.

**Cost**: ~10–20 formula BFS calls per hub per block. Formula BFS on 2000 pools takes ~5–10ms
each = ~100–200ms total.

**Multi-hub**: Run Phase 1 from each hub token (WAVAX, USDC, USDT, WETH.e). The pool sets
may overlap — union them all.

### Phase 2: EVM BFS (narrow, gas-aware)

**Goal**: Find the exact most profitable cycle with gas costs factored in.

**Algorithm**:
1. Build a **reduced adjacency graph** containing only the 20–40 pools from Phase 1.
2. Run BFS on this reduced graph, but each edge quote is an **EVM call** instead of a formula
   call. Each EVM call returns both `amountOut` and `gasUsed`.
3. Track per-node: `(amount, cumulative_gas)`.
4. Candidate scoring for a complete cycle: `profit = final_amount - start_amount - (total_gas × gasPrice)`.
5. The BFS naturally finds the path that maximizes actual profit, not gross output.

**Cost**: ~20–40 pools × 2 directions × up to 4 hops = ~100–300 EVM calls at ~0.1ms each =
~10–30ms. Completely feasible per block.

**TopK in Phase 2**: Not needed. With the reduced pool set and EVM precision, the top-1 by
profit-after-gas is the correct answer. TopK was an artifact of formula imprecision.

### Phase 3: Optimal Sizing via Binary Search

**Goal**: Find the input amount that maximizes profit for the winning cycle.

**Algorithm**:
1. Take the winning cycle from Phase 2.
2. Binary search on input amount: for each candidate amount, formula-quote the cycle (instant)
   and compute `profit = output - input - (estimated_gas × gasPrice)`.
3. Profit curve is concave (increases with amount until pool depletion). Binary search finds
   the peak in ~10–15 iterations.
4. Final EVM verification at the optimal amount to confirm exact profit and gas.

**Cost**: ~15 formula quotes (~microseconds) + 1 EVM call = negligible.

**Gas estimation for binary search**: Use the gas from Phase 2's EVM BFS as a constant estimate.
Gas doesn't change much with amount — it's mostly a function of hop count and pool types.

### Phase 4: Execution

**Same as arb1**: Sign and broadcast via `HayabusaRouter.swap()` with `minOutput = amountIn`.

Keep the executor from arb1 (`executor.go`) — it handles nonce management, gas pricing,
signing, WAVAX balance/allowance checks. The only change is the calldata comes from Phase 2/3
instead of arb1's scanner.

## Implementation Details

### Key files to reference

- **Pathfinder BFS**: `pathfinder/bfs.go` — `FindTopRoutes()` already supports cyclic mode
  (line 134: `cyclic := tokenIn == tokenOut`). When `tokenIn == tokenOut`, edges back to start
  at hop >= 1 become candidates. It does formula BFS + EVM verification of top candidates.
- **Adjacency graph**: `pathfinder/pools.go` — `BuildAdjacency(pools, registry)` builds
  `map[token][]PoolEdge`. For the reduced graph in Phase 2, build a new adjacency with only
  the discovered pools.
- **Pool loading**: `tools/pool-collector/embedded.go` — `EmbeddedPools(limit)` loads pools.
- **Formula engine**: `formulas/pool_quoter.go` — `PoolManager.Quote(pool, amount, tokenIn, tokenOut)`.
- **EVM execution**: `statedb/` — `CachedContext.ExecuteWithCallState()` for local EVM.
- **Calldata encoding**: `pathfinder/encode.go` — `EncodeSwapMulti()` for multi-hop swap calldata.
  Also see arb1's `verify.go` for the `swap()` selector with minOutput parameter.
- **State server**: Connect via `statedb.Connect(url)`. Use `LiveState.SetOnBlock()` for
  block-by-block processing. Dirty pool detection via `PoolManager.InvalidateBySlot()`.
- **Splitter's two-phase work**: `pathfinder/splitter/optimized2.go` — `CollectPaths()` and
  `AllocateAcrossPaths()` show the pattern of collecting paths then re-allocating. Similar
  concept applied here: discover pools, then optimize on the reduced set.

### Exclusion BFS implementation

The exclusion needs a way to run BFS with certain pools removed. Options:
1. **Filter adjacency**: Before each exclusion BFS, build a filtered adjacency that skips
   excluded pool indices. This is clean — just copy the adj map and remove edges.
2. **Blacklist parameter**: Add an optional `excludePools` set to `FindTopRoutes`. Cleaner API
   but modifies the pathfinder.

Recommend option 1 — keep the pathfinder untouched, filter adjacency at the call site.

### EVM BFS implementation

This is a new BFS variant that uses EVM instead of formulas for edge quoting. Options:
1. **Implement in arb4** as a standalone BFS on the reduced graph. Simpler, self-contained.
2. **Add to pathfinder** as an alternative quoter. More reusable but heavier change.

Recommend option 1 for now. The reduced graph is small enough (20–40 pools) that a simple
nested loop BFS is fine — no need for the full pathfinder machinery (topK beam, candidate
sorting, etc.).

The EVM BFS pseudo-code:
```
func evmBFS(pools, adj, state, cfg, router, sender, hub, maxHops, gasPrice):
    // Layer 0: start with hub token, probe amount
    // Each node: (amount, gasAccum, path)
    // Per hop: for each node, try all edges in reduced adj
    //   EVM-execute single-hop swap → get (outAmount, hopGas)
    //   New node: (outAmount, gasAccum + hopGas, path + edge)
    // Keep top-1 per token per layer (beam = 1, formulas are precise)
    // Candidates: nodes that return to hub with profit > gas
    // Score: amount - startAmount - gasAccum * gasPrice
    // Return: best candidate
```

### Block processing loop

Same event-driven model as arb1:
1. `LiveState.SetOnBlock()` callback fires with diff entries.
2. `PoolManager.InvalidateBySlot()` identifies dirty pools.
3. If any dirty pools → run Phase 1 → Phase 2 → Phase 3 → Phase 4.
4. All under `ls.RLock()` to ensure consistent state.

### Dirty pool optimization

Phase 1 runs full BFS even though only some pools changed. Optimization for later:
only re-run exclusion BFS if a dirty pool is in the current pool set. If no dirty pools
affect discovered cycles, skip directly to Phase 3 (re-size the existing winner with
updated state).

### Hub tokens

```go
var hubs = []common.Address{
    WAVAX,  // 0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7
    USDC,   // 0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E
    USDT,   // 0x9702230A8Ea53601f5cD2dc00fDBc13d4dF4A8c7
    WETHe,  // 0x49D5c2BdFfac6CE2BFdB6640F4F80f226bc10bAB
}
```

### Probe amounts for Phase 1

```go
// In hub token's native decimals. Cover 4 orders of magnitude.
// WAVAX (18 dec): 0.1, 1, 10, 100 AVAX
// USDC (6 dec): 2, 20, 200, 2000 USDC
// Scale to ~same USD value range for each hub.
```

### Command-line flags

```
--state-server <url>   WebSocket to state server (default: ws://localhost:7449/live)
--rpc <url>            HTTP RPC for live execution (default: http://localhost:9650/ext/bc/C/rpc)
--max-hops <n>         Max cycle hops: 2-4 (default: 4)
--pool-limit <n>       Pool count cap (default: 2000)
--execute              Live mode (default: dry-run)
```

### Output format

JSON lines to stdout (same as arb1):
```json
{"type": "opportunity", "block": 123, "profit_wei": "...", "hops": 3, "pools": [...], "amount": "..."}
{"type": "tx_sent", "block": 123, "txHash": "0x..."}
```

Structured stderr logging with `[arb4]` prefix for debugging.

## File structure

```
experiments/arb4/
├── PLAN.md          # this file
├── main.go          # entrypoint, block loop, flags
├── discover.go      # Phase 1: exclusion BFS, pool set discovery
├── evmbfs.go        # Phase 2: EVM BFS on reduced pool set
├── sizing.go        # Phase 3: binary search for optimal amount
├── executor.go      # Phase 4: copied from arb1, adapted
└── verify.go        # calldata encoding (shared with arb1 pattern)
```

## Success criteria

1. Finds all opportunities arb1 finds (no regressions).
2. Finds opportunities arb1 misses (cross-hub, better sizing).
3. Per-block latency < 500ms (Phase 1 + Phase 2 + Phase 3).
4. Gas-aware: never executes a cycle that's profitable gross but unprofitable after gas.
5. Profit per opportunity >= arb1 (optimal sizing vs fixed buckets).
