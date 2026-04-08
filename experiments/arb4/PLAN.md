# Arb4: Two-Phase BFS Arbitrage

## Context

Rewrite of the cyclic arbitrage scanner. Arb1 (`experiments/arb1/`) is the current production
scanner. Arb4 replaces it with a fundamentally better approach.

### What's wrong with arb1

- **Fixed size buckets (5 amounts)** — coarse. Optimal amount is almost never one of the 5.
- **Rate table screening** — ~7000 false positives per block (arb3 analysis). Float64 error.
- **No gas awareness until Phase 3** — optimizes gross output, not profit after gas.
- **WAVAX hub only** — misses cross-hub arb through USDC, USDT, WETH.e.
- **Static cycle enumeration** — doesn't adapt to which pools have liquidity shifts.

## Arb4 Architecture

### Key insight

Formulas are wei-level accurate (98%+ match rate with EVM). Use formula BFS to discover
*which pools* participate in profitable cycles (cheap, broad). Then use EVM BFS on just
those pools for exact gas measurement and side-effect-aware scoring.

---

### Phase 1: Exclusion BFS (formula, broad discovery)

**Goal**: Discover a diverse set of pools that participate in profitable cycles.

**Algorithm**:
1. Run `FindBestRoute(pm, adj, pools, state, cfg, router, sender, HUB, HUB, probeAmount, maxHops)`
   — cyclic mode (tokenIn == tokenOut) finds the best cycle.
2. Extract pools from that cycle (e.g., [A, B, C] for a 3-hop).
3. **Exclude**: For each pool in the winning cycle, build a filtered adjacency that removes
   edges for that pool. Run BFS again on the filtered graph:
   - Exclude A → best cycle → [D, B, E]
   - Exclude B → best cycle → [A, F, C]
   - Exclude C → best cycle → [A, B, G]
4. For each NEW pool found, exclude it in another round.
5. Stop after 2 rounds or when new paths are unprofitable (formula_output < input).
6. Repeat at multiple probe amounts (e.g., 0.1, 1, 10, 100 AVAX) to catch amount-sensitive paths.

**Output**: 20–40 unique pools participating in potentially profitable cycles.

**Why exclusion instead of topK**: TopK finds paths ranked by the same BFS beam — they share
most pools. Exclusion forces genuinely different routes through different parts of the graph.

**Cost**: ~10–20 formula BFS calls per hub per block × ~5–10ms each = ~100–200ms total.

**Multi-hub**: Run Phase 1 from each hub token (WAVAX, USDC, USDT, WETH.e). Union the pool sets.

**Implementation**: Filter adjacency at the call site — keep pathfinder untouched.

---

### Phase 1.5: Pricing Wave

**Goal**: Get a WAVAX price for every token in the reduced pool set, used for gas conversion
during Phase 2 intermediate node pruning.

**Algorithm**:
1. **Wave 1**: For every token in the reduced set that pairs with WAVAX in any pool,
   formula-quote `1 WAVAX → token`. Record `price[token] = quote_output / 1e18`.
2. **Wave 2**: For every still-unpriced token that pairs with a Wave 1 token,
   chain: `price[token] = price[bridge_token] × quote(1_unit_bridge → token)`.
3. **Wave 3**: One more layer for anything still unpriced.

**Cost**: ~40–80 formula quotes = microseconds.

**Result**: `map[token]float64` — how many units of each token equal 1 WAVAX.
Gas conversion: `gas_in_token = gas_avax × price[token]`.

For WAVAX hub, gas is already in AVAX — no conversion needed at cycle completion.
For non-WAVAX hubs, one formula quote `1 WAVAX → hub_token` gives the rate.

---

### Phase 2: EVM BFS (narrow, gas-aware)

**Goal**: Find the exact most profitable cycle with gas costs factored in.

**Key design**: Every EVM call is a **full multi-hop swap** from hub through the entire path,
executed as a single transaction via `EncodeSwapMulti` + `ExecuteWithCallState`. This gives:
- Accurate side effects (state changes from hop 1 affect hop 2 within the same tx)
- Total gas for the whole path in one measurement
- Exact output amount

**Algorithm**:
1. Build a **reduced adjacency graph** containing only the Phase 1 pools.
2. BFS layer by layer (up to maxHops):
   - **Layer 1**: For each pool adjacent to hub in reduced graph, execute 1-hop EVM swap.
     Get `(amountOut, gasUsed)`. Keep top-1 per intermediate token.
   - **Layer 2**: For each survivor from layer 1, extend with each adjacent pool. Execute
     the **full 2-hop path** as one EVM call. Get `(amountOut, totalGas)`. Keep top-1 per token.
   - **Layer 3, 4**: Same pattern — always execute the full path from hub.
   - At each layer, edges back to hub token (hop >= 2) are cycle candidates.
3. **Intermediate pruning**: To compare two paths arriving at the same token with different gas,
   use pricing wave: `net_value = amount - cumulative_gas × gasPrice × price[token]`.
   Keep the path with higher net value.
4. **Cycle scoring**: `profit = output_hub - input_hub - (totalGas × gasPrice)`.
   For non-WAVAX hubs, convert gas via the precomputed hub rate.

**Cost**: With ~20 pools and beam=1 per token:
- Layer 1: ~20 EVM calls
- Layer 2: ~15 survivors × ~3 edges = ~45 calls
- Layer 3: ~45 calls
- Layer 4: ~45 calls
- Total: ~155 EVM calls at ~0.1ms each = ~15ms

**TopK**: beam=1 is sufficient. Formulas are precise enough that the best amount at each
intermediate token is the right one. TopK was an artifact of formula imprecision.

---

### Phase 3: Optimal Sizing (binary search)

**Goal**: Find the input amount that maximizes profit for the winning cycle.

**Algorithm**:
1. Take the winning cycle (path) from Phase 2.
2. Binary search on input amount: for each candidate, formula-quote the cycle and compute
   `profit = output - input - (estimated_gas × gasPrice)`.
3. Profit curve is concave. Binary search finds the peak in ~15 iterations.
4. Final EVM verification at the optimal amount to confirm exact profit and gas.

**Gas estimate**: Use the gas from Phase 2 as a constant — gas is mostly a function of
hop count and pool types, not amount.

**Cost**: ~15 formula quotes (microseconds) + 1 EVM call = negligible.

---

### Phase 4: Execution

Same as arb1: sign and broadcast via `HayabusaRouter.swap()` with `minOutput = amountIn`.
Keep the executor from arb1 — it handles nonce management, gas pricing, signing.

---

## Hub Tokens

```go
var hubs = []common.Address{
    WAVAX,  // 0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7
    USDC,   // 0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E
    USDT,   // 0x9702230A8Ea53601f5cD2dc00fDBc13d4dF4A8c7
    WETHe,  // 0x49D5c2BdFfac6CE2BFdB6640F4F80f226bc10bAB
}
```

## Probe Amounts (Phase 1)

```go
// In hub token's native decimals. Cover 4 orders of magnitude.
// WAVAX (18 dec): 0.1, 1, 10, 100 AVAX
// USDC (6 dec): 2, 20, 200, 2000 USDC
// Scale to ~same USD value range per hub.
```

## CLI Flags

```
--rpc <url>            HTTP RPC for live execution (default: http://localhost:9650/ext/bc/C/rpc)
--max-hops <n>         Max cycle hops: 2-4 (default: 4)
--pool-limit <n>       Pool count cap (default: 2000)
--execute              Live mode (default: dry-run)
```

State server URL is hardcoded default `ws://localhost:7449/live`.

## Output Format

JSON lines to stdout:
```json
{"type": "opportunity", "block": 123, "hub": "WAVAX", "profit_wei": "...", "hops": 3, "pools": [...], "amount": "..."}
{"type": "tx_sent", "block": 123, "txHash": "0x..."}
```

Structured stderr logging with `[arb4]` prefix.

## File Structure

```
experiments/arb4/
├── PLAN.md          # this file
├── main.go          # entrypoint, block loop, flags
├── discover.go      # Phase 1: exclusion BFS, pool set discovery
├── pricing.go       # Phase 1.5: pricing wave for gas conversion
├── evmbfs.go        # Phase 2: EVM BFS on reduced pool set
├── sizing.go        # Phase 3: binary search for optimal amount
└── executor.go      # Phase 4: signing and broadcasting
```

## Key Dependencies

- `pathfinder.FindBestRoute` — formula BFS with cyclic mode
- `pathfinder.BuildAdjacency` — token→[]PoolEdge adjacency
- `pathfinder.EncodeSwapMulti` — multi-hop swap calldata
- `pathfinder.Pool`, `PoolEdge`, `RouteStep`, `Route` — data types
- `formulas.PoolManager` — formula quoting
- `statedb.LiveState`, `CachedContext`, `CallState` — state + EVM
- `contracts.BuildSenderOverrides` — EVM simulation overrides
- `poolcollector.EmbeddedPools` — pool database
