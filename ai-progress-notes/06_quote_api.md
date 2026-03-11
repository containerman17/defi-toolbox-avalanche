# Quote API: Decoupled Prepare + Lazy Quote

## Overview

The previous API required callers to specify all (token_in, token_out, size, max_hops) directions upfront in `prepare`. The new API decouples Phase 0+1 (expensive, global) from Phase 2 (cheap, per-direction, lazy).

## Protocol

### `set_pools`
Unchanged. Loads pool universe.

### `prepare` (Phase 0 + Phase 1 only)
```json
{ "op": "prepare", "id": 1, "sizes": ["100000000000000000", "1000000000000000000"] }
```
- `sizes`: AVAX-denominated size tiers in wei (e.g. 0.1 AVAX, 1 AVAX)
- Runs Phase 0 (token pricing via 2-wave BFS from WAVAX)
- Runs Phase 1 (rate table for every edge at each size tier)
- Clears Phase 2 cache (rates changed, old narrowing is stale)
- Response: `{ "op": "result", "phase0_ms", "phase1_ms", "phase0_evm", "phase1_evm" }`

### `quote` (Lazy Phase 2 + BFS)
```json
{ "op": "quote", "id": 2, "token_in": "0x...", "token_out": "0x...", "amount": "10000000", "max_hops": 4, "max_pools": 500 }
```
- `max_pools`: How many top-scored pools BFS should use (default 500). Phase 2 always keeps up to 1000 pools sorted by score; `max_pools` truncates at BFS time.

Processing:
1. **Tier snapping**: Convert `amount` (in token_in wei) to AVAX equivalent using Phase 0 prices. Snap to nearest tier from the prepared set. Error if no tier was prepared.
2. **Phase 2 (lazy)**: Check cache for `(token_in, token_out, snapped_tier, max_hops)`. On miss, run Phase 2 f64 enumeration (keeps top 1000 pools by score), cache result. On hit, reuse.
3. **BFS**: Take top `max_pools` from the scored list. Run BFS on those pools with exact `amount` and `max_hops`.
4. Response: same as old `bfs` response, plus `snapped_tier`, `phase2_us` (0 if cache hit), `phase2_cached`.

### `set_block`
Unchanged. Switches block. Does NOT invalidate prepare cache — caller must re-prepare if they want fresh rates.

## Size Tier Ladder

TypeScript defines the tier ladder (what to prepare):
```
0.01 AVAX, 0.1 AVAX, 1 AVAX, 10 AVAX, 100 AVAX, 1000 AVAX
```
The caller chooses which subset to prepare. `quote` snaps to the nearest prepared tier. If the snapped tier wasn't prepared, Rust returns an error.

## Phase 2 Cache

- Key: `(token_in: Address, token_out: Address, tier: U256, max_hops: usize)`
- Value: `NarrowedPools` (top 1000 pools by score, sorted)
- Invalidated: on every `prepare` call (Phase 0+1 results changed)
- max_hops is part of the key because Phase 2 with max_hops=2 discovers different pools than max_hops=4
- `max_pools` is NOT part of the key — it's applied at BFS time, so you can reuse the same cached narrowing with different BFS pool limits

## Example Flow

```
set_pools(all 7500 pools)
prepare(sizes: [0.1 AVAX, 1 AVAX])        # Phase 0+1, cold ~3s, hot ~300ms
prepare(sizes: [0.1 AVAX, 1 AVAX])        # hot run, print timing

quote(USDC → WAVAX, 10 USDC, max_hops=4)  # 10 USDC ≈ 0.3 AVAX → snaps to 0.1 AVAX tier
                                            # Phase 2 cache miss → run → cache
                                            # BFS on narrowed pools → route

quote(USDC → WAVAX, 8 USDC, max_hops=4)   # 8 USDC ≈ 0.24 AVAX → snaps to 0.1 AVAX tier
                                            # Phase 2 cache HIT (same direction+tier+hops)
                                            # BFS only → fast

quote(WAVAX → USDC, 50e18, max_hops=4)    # 50 AVAX → snaps to... error if 100 AVAX not prepared
```

## What's Removed

- `prepare` no longer accepts `directions`
- No `bfs` op (replaced by `quote`)
- No `narrow` op
- No backwards compatibility with old API
