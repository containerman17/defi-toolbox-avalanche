# Changelog

## 2026-03-24 — Go migration + formula engine

### Repo restructuring
- Converted all evm-quoter .mjs files to .ts
- Deduplicated RouteStep type — pathfinder imports from router/encode.ts
- Moved decodeSwapResult to router/encode.ts
- Unified buildStateOverrides in router/overrides.ts with routerAddress param
- Extracted ERC4626 vaults to pool-collector/erc4626.ts (browser-safe)
- Changed pathfinder + router/encode to import from pool-collector/types.ts directly (avoids discovery.ts Node deps)

### Browser demo
- Created sdk-browser.ts — browser WASM SDK (native WebSocket, fetch for WASM)
- Created examples/browser-quoting/ — Vite project, full BFS pathfinder in browser
- Fixed wasm_exec.js already browser-compatible (no polyfills needed)

### Bug fixes
- Fixed wsPool race condition: state-server initial_dump was consumed as RPC response
- Fixed BTC.b address checksum in ERC4626 vaults
- Fixed backrun benchmark: multi-swap pool pairing by log-index order

### Formula engine (Go)
- Added V2/LFJ V1 constant product formula — 3.2x faster for those pool types
- Added Pharaoh V1 formula (stable/volatile curves) — ~5x faster
- Added Uniswap V3 / Pharaoh V3 formula (tick-walking, uint256) — ~2.5x faster
- Speed: 0.911 → 0.476 ms/pool (-48%) with formulas
- Formula registry: 4239 validated pools, 345 invalid (FoT/broken)
- Discovery script auto-populates registry via EVM comparison (exact match only)

### JS formula experiment (abandoned)
- Tried JS BigInt formula quoting — worked for V2 but fundamentally wrong approach
- 0.1% tolerance was way too loose for DeFi (user corrected: must be exact)
- Formula in JS can't handle complex pool types (V3 tick-walking, Balancer hooks)
- Reverted all JS formula code — formulas belong 100% in Go

### Token override investigation
- Discovered pathfinder only set overrides for 4 starter tokens
- Missing overrides caused EVM to revert on intermediate hops (accidentally pruned BFS)
- Formula bypassed this "pruning" (no transfer simulation) → inconsistent results
- Fix: set overrides for ALL tokens in graph, but this expands BFS frontier significantly
- Conclusion: proper overrides needed, but pathfinder needs better pruning for wider search

### Go migration
- Moved go.mod to repo root, unified module name: defi-toolbox
- Merged state-server into root module (cmd/state-server/)
- Ported BFS pathfinder to Go (pathfinder/bfs.go, pools.go, encode.go)
- Added find_route method to native harness — one IPC call, Go does all quoting in-process
- find_route: ~840ms per route (500 pools, warm) — was ~4.5s with JS BFS + IPC

### Performance investigation
- Overlay allocation was the biggest bottleneck: creating fresh overlay per EVM call
- Fix: create overlay with overrides once, use thin NewOverlay() per call
- find_route: 2000ms → 840ms (2.4x faster) from this single change
- Same fix applied to IPC eth_call_batch: 0.399 → 0.355 ms/pool

### Embedding
- Embedded pools.txt via go:embed in pool-collector/pools.go
- Embedded registry.txt via go:embed in formulas/registry.go
- Embedded bytecode.hex + token_overrides.json in router/overrides.go
- Created cmd/benchmark — pure Go benchmark binary, no JS

### Current benchmark numbers
- IPC batch: 0.355 ms/pool (4000 pools)
- Go benchmark: 2.2 ms/quote (100 pools, both directions)
- Go find_route: ~840ms per route (500 pools)
- Formula: 0.44 ms/quote, EVM: 3.5 ms/quote (Go benchmark)
- Target (experiments prototype): 0.024 ms/quote (75k quotes in 1.78s, all formula, in-process)

### Performance investigation — L3 cache and map lookups
- Micro-benchmarked Go map lookup patterns:
  - Two-level map (current StateDB): 585ns/lookup
  - Flat map ([52]byte key): 137ns/lookup — 4.3x faster
  - StateDB direct: 693ns/read
  - StateDB via overlay (warm): 107ns/read (cached in overlay)
  - StateDB cold overlay: 1690ns/read (includes NewOverlay allocs)
- Built FastCache (flat map) for formula reads — 387k entries
  - Result: **no improvement** (90µs/q vs 70µs/q without). Worse because of double lookup on cache miss.
  - Conclusion: the StateDB already has slots cached from initial_dump. Two-level map overhead (~700ns) is small compared to actual formula computation (V3 tick walking, Pharaoh V1 Newton-Raphson).
  - Kept FastCache as utility but removed from hot path.
- TryQuoteDirect (skip ABI encode/decode): **slower** (1120ms vs 810ms). Closure allocations in dispatchFormula outweigh the 709ns encode savings.
- Internal timing breakdown for find_route (500 pools, WAVAX→USDC):
  - Formula: 323 quotes, 22.7ms (70µs/q)
  - EVM: 916 quotes, 708ms (773µs/q)
  - Overhead: ~80ms (graph, pruning, JSON)
  - EVM is 87% of total time

### Additional formula types added
- LFJ V2 Liquidity Book: 247 pools validated, 248 invalid. Speed: 0.355 → 0.348 ms/pool
- Algebra V1 Integral: 45 pools validated. Speed: 0.348 → 0.321 ms/pool
- Total formula coverage: 4722 validated, 593 invalid
- Progress from baseline: 0.911 → 0.321 ms/pool (2.8x faster)

### Skip invalid pools in BFS
- BFS now skips pools marked -1 in registry, avoiding wasted EVM calls
- 916 → 890 EVM calls per route
- find_route median: 756ms (500 pools)

### IPC batch overlay optimization
- Applied same single-overlay fix to IPC eth_call_batch path
- IPC speed: 0.399 → 0.355 ms/pool (before adding more formulas)

### Per-type speed analysis (current state)
- pharaoh_v3: 565ms (44% of total) — 131 formula at 3024µs/pool, 16 EVM
- lfj_v1: 171ms — 1432 formula, 90 EVM (FoT)
- lfj_v2: 170ms — 112 formula, 88 EVM (mismatches)
- uniswap_v3: 101ms — 222 formula at 366µs/pool, 6 EVM
- pangolin_v2: 65ms — 428 formula, 34 EVM (FoT)
- pharaoh_v1: 50ms — 251 formula, 32 EVM (FoT)
- arena_v2: 44ms — 501 formula, 0 EVM
- Remaining (dodo, balancer, etc.): ~30ms total — not worth adding formulas

Key finding: pharaoh_v3 formula (ERC-7201 layout) is 8.3x slower than uniswap_v3 (standard layout) despite using the same tick-walking algorithm. ERC-7201 slot computation overhead.

### DODO PMM formula
- 14 pools validated, 4 invalid
- Speed: 0.321 → 0.307 ms/pool
- Total: 0.911 → 0.307 ms/pool (2.97x faster from baseline)
- Formula coverage: 4736 validated, 606 invalid

### Current state summary
| Metric | Baseline | Current | Change |
|--------|----------|---------|--------|
| IPC batch speed | 0.911 ms/pool | 0.307 ms/pool | **2.97x faster** |
| find_route (500 pools) | ~4500ms (JS BFS) | ~756ms (Go BFS) | **5.9x faster** |
| Formula coverage | 0 pools | 4736 pools | — |
| Correctness | 100% | 100% | unchanged |

### Pharaoh V3 deep dive
- 3024µs/pool — 8.3x slower than uniswap_v3 (366µs/pool)
- Hot path uses BytesStateReader (no string conversions) — verified
- The slowness is genuine tick-walking: pharaoh_v3 pools have wider tick ranges
- ERC-7201 slot computation causes more keccak cache misses per read
- This is the natural floor for V3 formula performance
- Potential optimization: BFS pruning to avoid exploring pharaoh_v3 when better routes exist

### Pre-computed startup
- find_route now uses embedded pools + pre-computed overrides — zero JSON parsing per call
- Pools, graph, and overrides computed once at Go binary startup
- DODO formula panics on certain pool states — added panic recovery in dispatchFormula

### Formula-vs-reality mismatch (known issue)
- Formula quoting succeeds for pools where actual swaps would fail
- BFS with all-token overrides finds routes with billions of % return
- Root cause: formula only does math on reserves, doesn't simulate token transfers
- This means the pathfinder finds routes through broken/paused/max-transfer-limited tokens
- Need: either (a) formula checks for known bad tokens, or (b) EVM verification of top routes
- The old JS BFS avoided this by only setting overrides for 4 starter tokens

### LFJ V2 formula bug
- Pool 0xa96bfdfa returns 1.2e27 output for small input — clearly wrong at non-validation amounts
- Root cause: formula discovery only tests pools from loadPools (starter-token pairs)
- BFS graph includes ALL pools from parsePools — many untested pools have broken formulas
- Fix needed: discovery must test all pools in the graph, not just the benchmark subset

### Open questions
- Discovery coverage gap: test ALL pools, not just starter-token pairs
- BFS pruning: can we skip slow pool types when faster alternatives exist?
- Formula-reality gap: need EVM verification for the final route
- BFS with all-token overrides explores 5000+ quotes per direction vs 1400 with 4-token overrides
- Pathfinder needs beam width / pruning for wider search
- LFJ V2 formula not implemented yet (335 pools, 441ms — next formula target)
