# BFS Routing

## Overview

Layer-by-layer BFS that finds the optimal swap path from token_in → token_out through up to 4 intermediate hops. Every pool is a black box — we execute an EVM call to get each quote. Quotes within a layer are parallelized via rayon.

## Data Structures

- `frontier: HashMap<Token, (amount, cumulative_gas)>` — best amount reaching each intermediate token
- `backtrack: Vec<HashMap<Token, (pool, from_token, amount_in, amount_out)>>` — per-layer info to reconstruct the winning path
- `best_at_layer: Vec<Option<(path, amount_out, gas)>>` — best route reaching token_out at each depth
- `pools_by_token: HashMap<Token, Vec<PoolEdge>>` — adjacency list

## Algorithm

1. Build backward reachability map from token_out (which tokens can reach it in N remaining hops)
2. Per layer:
   - For each (token, amount) in frontier, look up all pool edges
   - Skip edges back to token_in (no loops to start)
   - Skip edges whose output can't reach token_out in remaining hops (reachability pruning)
   - Execute all quotes in parallel via rayon
   - Terminal edges (output == token_out): full-path re-quote via single swap() call, compare against best_at_layer
   - Intermediate edges: keep best amount per output token in next_frontier
3. Pick best result across all layers (1-hop can beat 3-hop after gas)
4. Reconstruct path from backtrack maps

## Gas-Aware Scoring

"Better" = higher `amount_out - gas * basefee` converted to common denomination. Shorter paths with slightly less output can win if gas savings outweigh the difference.

For v1: just maximize amount_out, ignore gas. Add gas-aware scoring later.

## Full-Path Re-quote

BFS quotes each hop independently. But in a real transaction, state changes from hop N affect hop N+1. When an edge reaches token_out, reconstruct the full path and run it as a single swap() call to get exact amounts. This happens for ALL terminal candidates, not just the winner, to ensure correct ranking.

## Pool List

Pools are passed from TypeScript via the ndjson protocol in the BFS request. No pool selection logic in Rust — the frontend decides which pools to include.

## Optimization Notes

### StateSnapshot: lock-free reads, zero synchronization

The hot path has zero mutexes. Architecture:

- `StateSnapshot` is an immutable `Arc<HashMap<...>>` shared across all rayon threads
- Each thread gets its own `SnapDB` with local `Vec` for cold misses (no shared state)
- Read path: storage overrides → snapshot (lock-free) → RPC fetch (cold miss collected in Vec)
- After each `quote_all` / `quote_routes_parallel` batch, cold misses are merged into a new snapshot via `Arc::new(clone + merge)`

### Critical: collect fetched data from ALL EVM paths

Both `execute_quote_job` (quoteMulti for single-hop) and `execute_route` (swap for full-path re-quote) must extract `fetched_storage` and `fetched_accounts` from the SnapDB after the EVM call. Initially `execute_route` didn't do this — terminal route state was never cached, causing every warm run to re-fetch from RPC. Fix: return fetched Vecs from both functions, aggregate in `quote_routes_parallel`, merge into snapshot.

### Performance results (100 pools, 285 EVM calls, 4 hops)

| Run | Time | Calls/sec | Notes |
|-----|------|-----------|-------|
| Cold (first) | 37s | 8 | RPC-bound, ~1900 storage slots + 230 accounts fetched |
| Warm (second) | 12ms | 23,750 | All snapshot hits, zero RPC |
| Warm (third) | 10ms | 28,500 | Same |

Cold run cost is amortized — prototype 31 ran as a persistent server, warming up once per block.

### HashMap hasher for StateSnapshot storage
The snapshot uses `HashMap<Address, HashMap<B256, B256>>` for storage slot lookups. During BFS, thousands of EVM calls each hit storage multiple times — per-slot read latency dominates. Current plan: use ahash (same as revm internals). **Benchmark idea**: try FxHashMap, Swiss tables, or even a flat sorted Vec with binary search to see if better cache locality helps. In Go, swapping the map implementation gave ~2x speedup on similar workloads. Rust has no GC so the gains may differ — worth measuring.
