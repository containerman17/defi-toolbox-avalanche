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

### Open questions
- Go benchmark EVM is 4.5x slower than IPC EVM — investigating
- BFS with all-token overrides explores 5000+ quotes per direction vs 1400 with 4-token overrides
- Pathfinder needs beam width / pruning for wider search
- LFJ V2 formula not implemented yet (335 pools, 441ms — next formula target)
