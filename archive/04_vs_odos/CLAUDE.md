# 04_vs_odos — Beat Odos on Every Quote

## Goal

Our BFS router MUST produce better quotes than Odos on every single pair, every single time. This is the gating requirement — no new features, no speed optimizations, no block watching, no complexity until this is achieved.

## Why 0.15%?

Odos charges ~0.15% commission on swaps. This means even if our raw routing is merely *equal* to theirs, the user gets 0.15% more through us because we don't take a cut. So "equal routing quality" = "we win by 0.15%". Ideally we match or exceed their routing quality outright, making the gap even larger.

## Comparison methodology

All comparisons MUST use RPC-simulated amounts (`eth_call` with state overrides), never API-claimed values. Odos API can claim whatever it wants — the only truth is what the EVM actually returns when you simulate their assembled transaction on-chain.

Both our route and Odos's assembled tx are verified on the **exact same block** to eliminate timing skew.

## Two-script workflow

1. **`capture_odos.ts`** — Captures Odos quotes (quote + assemble + simulate) and saves to `captures/<block>.json`. This is a one-time capture per block. Odos cannot be queried for past blocks, so we freeze their output here.

2. **`race.ts`** — Loads a capture file, runs our BFS on the same block, verifies both via RPC. Deterministic and repeatable — keep perfecting our engine against the same frozen Odos snapshot until we win on every pair. Then capture a new snapshot and repeat.

## State caching

Set `STATE_CACHE_DIR` (defaults to `./state_cache`) to cache EVM state per block. The cache is additive — each run dumps the full snapshot including previously cached data plus newly fetched slots. This makes repeat runs near-instant (no cold RPC fetches).

## Correctness first, speed second

- Re-quoting (frontier re-quotes + terminal precision loop) is ALWAYS enabled in this example. Never disable it here.
- When investigating a losing pair: check which pools Odos uses that we don't, check if our pool list covers those DEXes, check if oracle-sharing corruption ate our best route.

## What to do when Odos wins

1. Look at the Odos pathViz to see which DEX/pool they routed through
2. Check if that pool exists in our pool list (`pools/data/pools.txt`)
3. If missing: add the pool provider or the specific pool
4. If present: investigate why our BFS didn't find that route (pruning? oracle corruption? hop limit?)
5. Re-run `race.ts` against the same capture to verify the fix

## Debugging strategy: pool isolation test

When investigating a losing pair, narrow it down with the `quote_with_pools` API:

1. **Isolate the competing pool**: Create a minimal pool list with ONLY the pool Odos uses (from trace). Send it to the Rust quoter. If `amount_out = 0`, the pool type quoting is broken.

2. **Isolate our pool**: Create a pool list with ONLY our chosen pool. Verify it returns what we expect.

3. **Both pools**: Send both. BFS should pick the better one. If it picks the worse one, the BFS comparison logic has a bug.

This is done via `test_v4.ts` in this folder — a quick script that sends custom pool strings to the Rust quoter to test specific pool types in isolation.

Example finding: V4 USDC/USDt pool returned `amount_out=0` — the V4 quoting in the Rust EVM was completely broken, which is why BFS never picked V4 pools.

## V4 quoting bugs found and fixed

Three bugs in `HayabusaRouter.sol` prevented all Uniswap V4 pools from working:

1. **Missing `unlockCallback(bytes)`** — The V4 PoolManager (`0x06380C0e0912312B5150364B9DC4542BA0DbBc85`) calls `unlockCallback(bytes)` (selector `0x91dd7346`) on `msg.sender` during `unlock()`. Our contract only had `v4UnlockCallback()` with a different selector, so the callback silently failed. Fix: added `unlockCallback` that dispatches via `delegatecall` to preserve `msg.sender` (needed because `v4UnlockCallback` has a `require(msg.sender == V4_POOL_MANAGER)` check).

2. **Wrong `amountSpecified` sign** — V4 uses negative values for exactIn swaps. We were passing `int256(_v4AmountIn)` (positive = exactOut). Fix: changed to `-int256(_v4AmountIn)`.

3. **Delta sign convention** — On Avalanche's V4, the packed `BalanceDelta` return from `swap()` uses: positive = amount caller receives, negative = amount caller pays. Our code assumed the opposite (negated both deltas). Fix: removed negation from `amountOut = uint256(uint128(delta))`.

### Debugging methodology used

- **Pool isolation via `test_v4.ts`**: sent V4-only pool list to BFS → `amount_out=0` confirmed V4 broken
- **`debug_traceCall` with bytecode override**: traced the full call tree via `eth_call` with quoter bytecode injected at `0xDEADBEEF`. This showed:
  - Bug 1: PoolManager's CALL back to our contract had no matching function selector
  - Bug 2+3: `V4_POOL_MANAGER.take()` reverted with `SafeCastOverflow()` (`0x93dafdf1`) because the decoded `amountOut` was `~2^128` instead of the real output amount
- **4byte.directory lookup**: identified `0x93dafdf1` as `SafeCastOverflow()`, pointing to the delta decoding issue
- **Raw swap return value analysis**: decoded `0x00000000000000000000000000988cd8ffffffffffffffffffffffffff676980` to find delta0=+9,997,528 (receive USDt) and delta1=−10,000,000 (pay USDC), revealing the sign convention

### Key V4 pools used in testing

- V4 USDC/USDt pool: `0xfe74ff9963652d64086e4467e64ceae7847ebf01` (fee=18, tickSpacing=1, hooks=0x0)
- V3 USDC/USDt pool for comparison: `0x1150403b19315615aad1638d9dd86cd866b2f456`
- V4 PoolManager: `0x06380C0e0912312B5150364B9DC4542BA0DbBc85`

Result: V4 pool gives 9,997,487 vs V3's 9,997,451 for 10 USDC→USDt (V4 wins by 36 units). BFS now correctly picks V4 when available.

### Race results at block 80341389 (after V4 fix)

- **6 wins**: USDC→WAVAX, WAVAX→USDC, AVAX→USDt, USDt→WAVAX (all +0.03–0.04%)
- **2 ties**: USDC→USDt (both use V4, identical output)
- **2 losses**: AVAX→sAVAX (−0.005%/−0.002%) — Odos routes through Balancer V3 Vault (`0xba1333...`) with ERC-4626 wrappers (WAVAX→waAvaWAVAX→Balancer→waAvaSAVAX→sAVAX). We don't have this multi-hop ERC-4626+Balancer route.

### Race results at block 80341389 (after Balancer V3 Buffered fix)

- **6 wins**: +USDC→WAVAX, WAVAX→USDC, AVAX→USDt, 1 AVAX→sAVAX (+0.000006%)
- **2 ties**: USDC→USDt
- **2 losses**: 10 AVAX→sAVAX (−0.0001%, Odos splits 83%/17%), 50 USDt→WAVAX (−0.003%)
- **Win rate: 75%** (up from 62.5%)

## Balancer V3 Buffered pool type (type=11) — the sAVAX breakthrough

Odos was beating us on WAVAX→sAVAX and sAVAX→WAVAX by routing through Balancer V3's buffer system: wrap→swap→unwrap inside a single `Vault.unlock()` callback. Our 3-hop approach (WAVAX→waAvaWAVAX→waAvaSAVAX→sAVAX) lost because each hop executed independently, causing Aave state changes between steps that degraded the price.

### Fix: BALANCER_V3_BUFFERED pool type

Added pool type 11 (`BALANCER_V3_BUFFERED`) to `HayabusaRouter.sol` that executes wrap+swap+unwrap atomically inside a single `Vault.unlock()` callback, using `erc4626BufferWrapOrUnwrap` for wrap/unwrap and `Vault.swap()` for the pool swap — all settled via Vault's transient storage (no intermediate token transfers).

### Test results at block 80341389 (1 AVAX → sAVAX)

- **Our amount:  798126922778014716** (0.798126922 sAVAX)
- **Odos amount: 798126872444040064** (0.798126872 sAVAX)
- **Diff: +0.000006%** — we beat Odos

Both `quoteMulti` and `quoteRoute` return identical values, confirming consistency.

## Do NOT

- Do not add block watching, state diffing, or any live-following features
- Do not optimize for speed until we beat Odos on all pairs
- Do not increase complexity (top-K frontier, split routing, etc.) unless a specific losing pair requires it
- Do not advance to new examples/prototypes until this benchmark is green
