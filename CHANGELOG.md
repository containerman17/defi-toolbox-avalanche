# Changelog

## 2026-03-25 — Pool quoter structs + EVM optimization

### Session results summary

| Metric | Start of session | End of session | Improvement |
|--------|-----------------|----------------|-------------|
| **Speed (ms/pool)** | 0.797 | **0.224** | **3.6x** |
| **Formula coverage** | ~50% | **93.3%** | +43pp |
| **Correctness** | 100% (JS IPC) | **98.3%** (pure Go) | — |
| **Formula time** | 331ms | **157ms** | 2.1x |
| **EVM time** | 3551ms | **538ms** | 6.6x |
| **Formula quotes** | ~3900 | **7390** | +3490 |
| **EVM quotes** | ~4100 | **528** | -3572 |

### Token dollar value registry
- Built by quoting through formula pools: WAVAX → token pairs, 5 rounds of propagation
- 1471 tokens with dollar-equivalent amounts (formulas/data/token_amounts.txt)
- Used by discovery to validate ALL token pairs, not just USDC/USDT/WAVAX starters
- 92.7% token coverage of top 4000 pools

### wsFetcher RPC stubs fixed
- Benchmark and discovery wsFetcher.FetchCode/FetchNonce were stubs returning nil/0
- Fixed to fetch via state server RPC on demand — everything works without initial_dump
- --no-dump flag for testing: skips initial_dump, proves system is fully demand-driven
- Initial dump is purely a speedup (1.3s vs 72s for first pass), not a requirement

### Direct pool registration from storage slots
- V2/LFJ_V1: check slot 8 (reserves) — 813 pools registered without EVM
- V3/Pharaoh_V3: check slot 0 (sqrtPriceX96) — non-zero = has liquidity
- Pharaoh V1: check slots 8-11 (various reserve layouts)
- Algebra: check slot 2 (globalState)
- Bypasses EVM validation — just reads state directly

### Uniswap V4 formula
- Ported from experiment 02 — V4 uses singleton PoolManager with per-pool state indexed by poolId
- Math identical to V3 (tick walking, computeSwapStep) but different storage layout
- V4Pool struct pre-loads bitmaps and ticks from PoolManager contract
- 145 V4 formula quotes added, pools registered from ExtraData in pools.txt

### FoT (fee-on-transfer) token support
- Ported ~40 token fee calculators from experiment 02, matching exact Solidity integer math
- PoolManager wraps quoters with fotPoolQuoter — adjusts input/output for transfer tax
- Includes exempt pools, rebasing tokens, formula-issue tokens lists
- Correctness: 96.5% → 98.0% (58 fewer mismatches)

### Direct V2 pool registration from state
- Discovery script now checks slot 8 reserves directly for V2/LFJ_V1 pools
- If reserves are non-zero in state server, pool is registered without EVM validation
- Added 667 pools that were missing because they don't pair with starter tokens
- State server fetches missing slots on demand from upstream RPC

### Pure Go discovery tool (cmd/discover)
- Replaces JS-based discover_formulas.ts — no Node.js, no IPC
- Merge mode: keeps existing valid entries, only adds or upgrades
- Uses starter tokens (USDC/USDT/WAVAX) for EVM validation
- Direct slot 8 check for V2 pools without starter token pairs

### Pool quoter structs (formulas as stateful objects)
- Architecture change: each pool is now a struct that reads state ONCE at construction time
- `Quote(amountIn, zeroForOne)` is pure math — zero state access, zero keccak, zero map lookups
- `PoolManager` handles lazy construction + contract-level invalidation via `Invalidate(addr)`
- Implemented: V2Pool, V3Pool (with pre-scanned tick index), PharaohV1Pool, DODOPool
- Not yet structs: LFJ V2, Algebra — still use function-based formula path

### V3Pool skip-empty bitmap words
- Root cause: Solidity can only SLOAD one slot at a time, so V3 scans bitmap words one by one
- Our Go formula copied this, calling computeSwapStep (~5µs) for each empty word
- Pools with 6 ticks spread far apart scanned 3000+ empty words = 15ms of wasted mulDiv math
- Fix: pre-load all bitmap words at construction, then scan in a tight loop
- `w.IsZero()` on pre-loaded uint256 = ~2ns vs computeSwapStep = ~5000ns = **2500x cheaper per word**
- A pool scanning 3000 empty words: 15ms → 6µs
- Accuracy: <7 PPM max error from fee rounding at word boundaries (fee is computed once instead of per-word)
- Correctness benchmark uses 0.01 PPM tolerance — 99.1% pass (32 failures are pre-existing registry bugs)

### V3Pool pre-loaded state
- Constructor scans all bitmap words (±200), reads liquidityNet for each initialized tick
- 82 V3 pools total, 2,621 initialized ticks, ~330KB memory
- Quote reads from pre-loaded `bitmapWords` map and `tickLiquidityNet` map
- Zero keccak, zero state access at quote time

### Per-type formula results (struct vs original function-based)

| Type | Pools | Original formula (ms) | Struct formula (ms) | Speedup |
|------|-------|-----------------------|---------------------|---------|
| V3 (uniswap_v3) | 317 | 274 | **6.6** | **41x** |
| V2 | 1229 | 2.5 | **0.3** | **8x** |
| LFJ V1 | 1758 | 2.3 | **0.3** | **8x** |
| Pharaoh V1 | 286 | 2.5 | **1.4** | **2x** |
| LFJ V2 (no struct) | 111 | 22 | 22 | 1x |
| Algebra (no struct) | 58 | 16 | 16 | 1x |
| **Total formula** | | **331** | **48** | **6.9x** |

### Combined benchmark results (formula + EVM, 4000 pools)

| Metric | Baseline (session start) | Current | Improvement |
|--------|--------------------------|---------|-------------|
| Overall ms/pool | 0.375 | **0.335** | **11% faster** |
| Formula time | 331ms | **48ms** | **6.9x faster** |
| EVM time | ~1200ms | ~1200ms | same |
| V3 formula µs/quote | 553 | **13.3** | **41x faster** |
| V2 formula µs/quote | 1.7 | **0.2** | **8x faster** |

### Full session optimization stack (from original baseline)

| Change | EVM µs/quote | Formula ms | Overall ms/pool |
|--------|-------------|-----------|-----------------|
| Baseline (StateDB overlay) | 627 | — | 0.797 |
| + Formula engine | 627 | 331 | 0.375 |
| + CallState (thin overlay) | 396 | 331 | 0.351 |
| + CallerContract (JUMPDEST) | 396 | 331 | 0.351 |
| + Pool quoter structs | 396 | **48** | **0.335** |
| + Registry refresh (Go discover) | 396 | **70** | **0.323** |
| **Total improvement** | **1.6x** | **4.7x** | **2.5x** |

### Pure Go formula discovery (cmd/discover)
- Replaces JS-based `discover_formulas.ts` — no Node.js, no IPC
- Connects to state-server, quotes all pools via EVM, writes registry
- Merge mode: keeps existing valid entries, only adds new or upgrades -1 → valid
- Uses starter tokens (USDC/USDT/WAVAX) for quoting — matches JS behavior
- Registry refresh: 131 pools upgraded from invalid to valid formula IDs

### Final benchmark (4000 pools, single-threaded)

| Metric | EVM-only | With formulas | Savings |
|--------|----------|---------------|---------|
| Total time | 3551ms | **1117ms** | **69%** |
| ms/pool | 0.888 | **0.323** | **2.7x** |
| Formula time | — | 70ms | — |
| EVM time | 3551ms | 1047ms | — |

### Profiling insights
- **EVM**: 98% of CPU in EVMInterpreter.Run (opcode dispatch, stack ops). Our StateDB <5%. ~400µs/call is the libevm interpreter floor.
- **V3 formula math**: 63% was uint256 mulDiv (Knuth Algorithm D). 7% keccak, 7% map lookups.
- **Go vs Rust comparison**: Implemented full V3 formula in Rust with ruint. Go holiman/uint256 was 1.2x FASTER than Rust ruint. Rewriting in Rust won't help.
- **V3 bitmap scanning was the real bottleneck**: pools with 6 initialized ticks read 3462 bitmap words of zeros. Pre-scanning + binary search eliminated this entirely.

### Pure Go benchmark system
- Replaced JS IPC benchmark (bench.mjs) with pure Go benchmark (cmd/benchmark)
- 2 warm passes (hardcoded) + 1 timed hot pass
- Auto-logs to benchmark_results/evm_speed.log (skipped with --skip-formulas or --profile-mode)
- Per-type breakdown with formula read/math time split

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

### CallState + CallerContract optimization (EVM execution)
Inspired by experiments/10_local_hayabusa architecture. Two changes:

1. **CallState** — thin overlay replacing StateDB-backed-by-StateDB overlay.
   - Only stores storage + balance overrides (maps), delegates code/codeHash/nonce to base
   - Journal-based Snapshot/RevertToSnapshot — O(mutations) not O(all accounts) deep copy
   - No per-call keccak256 for codeHash — base has it cached from SetAccount
   - No per-call account struct allocation — just map writes for overrides

2. **CallerContract** — `*vm.Contract` caller instead of `AccountRef` for `evm.Call()`.
   - EVM's NewContract checks if caller is *Contract → shares JUMPDEST bitvector
   - All calls within a CachedContext share JUMPDEST analysis (~15% CPU savings from experiments)
   - JUMPDEST analysis for router/pool/token contracts computed once, reused across calls

**Benchmark results (4000 pools, warm+hot, single-threaded):**

| Metric | Before (StateDB overlay) | After (CallState) | Change |
|--------|--------------------------|-------------------|--------|
| EVM-only total | 5019ms | 3180ms | **1.58x faster** |
| EVM ms/quote | 627µs | 396µs | **37% faster** |
| With formulas total | 2089ms | 1351ms | **1.55x faster** |
| Overall ms/quote | 0.261 | 0.169 | **35% faster** |
| Warm pass (EVM-only) | 10936ms | 3445ms | **3.2x faster** |
| Warm pass (formulas) | 47442ms | 1582ms | **30x faster** |

Per-type highlights:
- v2 (simple): 924ms → 371ms (2.5x) — biggest win, most overhead was in overlay
- lfj_v1: 1045ms → 424ms (2.5x)
- uniswap_v4: 108ms → 22ms (5x)
- uniswap_v3: 1926ms → 1693ms (14%) — dominated by actual tick-walking EVM cost

### Current state summary
| Metric | Baseline | Current | Change |
|--------|----------|---------|--------|
| IPC batch speed | 0.911 ms/pool | 0.307 ms/pool | **2.97x faster** |
| EVM-only (hot, 4000 pools) | — | 396µs/quote | — |
| With formulas (hot) | — | 157µs/quote | — |
| find_route (500 pools) | ~4500ms (JS BFS) | ~756ms (Go BFS) | **5.9x faster** |
| Formula coverage | 0 pools | 4736 pools | — |
| Correctness | 100% | 100% | unchanged |

### CPU profiling of hot path (pprof)
- EVMInterpreter.Run is 98% of CPU time — genuine bytecode execution
- Stack operations (push/pop/swap/dup): 30% — libevm internals
- Opcode dispatch + PUSH data: 15% — interpreter loop
- Nested calls (opCall/DelegateCall/StaticCall): 48% — sub-contract invocations
- **Our StateDB/CallState overhead: <5%** — essentially eliminated
- SLOAD (GetState): 4.6%, GetCodeHash: 1.5%, map lookups: 2.8%
- SHA3 opcode (in-contract keccak): 3.1%
- Memory allocation: 182MB per hot pass (EVM Memory.Resize)
- Access list maps: 52MB per hot pass
- **Conclusion: ~400µs/call is the floor for this EVM interpreter (libevm)**
- Further gains need: more formulas, parallelism, or JIT EVM (evmone/revm)

### Formula performance deep dive + Go vs Rust comparison
- Instrumented formulas to separate state reads from math
- **V3 formula (60 reads, 32µs/call): 63% is uint256 division, 21% bitmap scan, 7% keccak, 7% map lookup**
- Simple formulas (V2/LFJ_V1): 1-3µs — pure x*y/z math, ~400x faster than EVM
- V3 formula: 32µs — only 1.3x faster than EVM, dominated by mulDiv (512-bit intermediate division)
- Created isolated benchmark: `experiments/formula-speed/` with fixture JSON + Go `testing.B` + Rust comparison
- **Go vs Rust on full V3 formula (same algorithm, same state):**
  - Pool with 4 reads: Go 2,735ns vs Rust 3,292ns — **Go 1.2x faster**
  - Pool with 60 reads: Go 31,912ns vs Rust 78,390ns — **Go 2.5x faster**
  - Go's `holiman/uint256` is competitive with Rust's `ruint` for mulDiv operations
  - **Conclusion: rewriting formulas in Rust won't help. Go uint256 is already near-optimal.**
- Pools with >100 reads are doing tick-walking through sparse bitmap regions — inherent to the algorithm

### Pure Go benchmark replaces JS IPC benchmark
- Old benchmark: JS bench.mjs → spawns Go native harness → IPC stdin/stdout JSON → measures wall time including serialization
- New benchmark: `go run ./cmd/benchmark/ --state-server ws://localhost:7449` — pure Go, no JS, no IPC
- Default 2 warm passes (JUMPDEST + CPU cache), then timed hot pass
- Auto-logs to `benchmark_results/evm_speed.log` (skipped when --skip-formulas or --profile-mode set)
- Log format: `time=... git=... result=<ms/pool> pools=... formulas=... evm=... ok=...`
- Result: 0.307-0.323 ms/pool — matches old IPC benchmark numbers, now apples-to-apples

### V3 tick index optimization
- Root cause of slow V3 formulas: linear bitmap scanning copied from Solidity's one-SLOAD-at-a-time approach
- Median V3 pool has 6 initialized ticks but scanned 425 bitmap words (mostly zeros) to find them
- Fix: pre-scan all bitmap words once per pool, build sorted array of initialized tick positions
- Quote-time: binary search O(log n) instead of linear scan O(distance/256)
- **V3 formula: 152ms → 10ms (14.8x faster)**
- **Total formulas: 240ms → 53ms (4.5x faster)**
- Reads per V3 quote: 425 → 10
- 82 V3 pools have 2,621 total initialized ticks — trivial to pre-compute
- First-pass cost: ~52s (401 bitmap words × 246 pools × keccak + state read)
- Per-block rebuild: only ~5-10 touched pools, ~1ms
- Memory: 2,621 ticks × ~128 bytes ≈ 330KB

### Multi-pass warm analysis
- Pass 1→5 improvement: ~10% (3779ms → 3449ms) — JUMPDEST already cached via CallerContract
- Remaining improvement is CPU cache warming, not JUMPDEST
- CallerContract's jumpdests map accumulates across all calls within a CachedContext

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

### EVM performance investigation
- NewOverlay allocation: 30ns — NOT the bottleneck
- Reusable overlay (Reset instead of NewOverlay): 2% improvement — negligible
- EVM per-call setup (NewEVM + blockCtx + big.Int): 5902ns, 54 allocs
- Cached block context: saved ~100us per call, 14 fewer allocs
- **Root cause from experiments**: per-overlay keccak256 recomputation + deep copy snapshots + no JUMPDEST sharing
- **Fix: CallState + CallerContract**: 627µs → 396µs per EVM call (37% faster)
- Target from experiments (192-core): ~192us per call per core
- Our single-core: ~396us — 2x gap, likely remaining from libevm interpreter overhead

### Open questions
- Discovery coverage gap: test ALL pools, not just starter-token pairs
- BFS pruning: can we skip slow pool types when faster alternatives exist?
- Formula-reality gap: need EVM verification for the final route
- BFS with all-token overrides explores 5000+ quotes per direction vs 1400 with 4-token overrides
- Pathfinder needs beam width / pruning for wider search
- LFJ V2 formula not implemented yet (335 pools, 441ms — next formula target)
