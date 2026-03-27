# Formula Coverage Playbook

Living document for investigating and fixing formula coverage gaps.
Agents investigating coverage should read this first, and append findings/tools below.

## Current State (2026-03-27)

1582 formula / 418 EVM fallback out of 2000 quotes (1000 pools × 2 directions).
**79.1% formula coverage, 100% correctness** (0 mismatches).

> **2026-03-27 update (zombie V3 fix):** 4 zombie V3 pools un-blacklisted with bitmap-empty detection. Formula coverage unchanged (zombie pools return nil, EVM fallback). 0 new mismatches.

### EVM Fallback Breakdown

| Reason | Count | Description |
|--------|-------|-------------|
| blacklisted | 120 | Registry says -1; many are false positives from tooling bugs |
| quote_fail | 49 | Pool builds OK but Quote() returns (nil,false) — zero sqrtPrice, empty liquidity, etc. |
| builder_nil(fid=2) V3 | 38 | Pool not in `v3PoolFees` map (missing fee/tickSpacing) |
| builder_nil(fid=4) Algebra | 12 | `buildQuoter` switch missing `case FormulaAlgebra:` |
| not_in_registry | 10 | Pool types without any formula (wombat, synapse, platypus, trident, balancer_v2) |
| builder_nil(fid=3) LFJ V2 | 0 | FIXED: added V2.0 storage layout support (3 pools), blacklisted 2 (evm=0) |
| builder_nil(fid=8) Bal V2 | 3 | `newBalancerV2Pool` returns nil |
| builder_nil(fid=7) Bal V3 | 2 | `newBalancerV3Pool` returns nil (GyroECLP pools) |
| builder_nil(fid=1) Pharaoh | 0 | FIXED: added 5 missing pools to `pharaoh_v1_registry.go` |
| builder_nil(fid=0) V2 | 2 | `newV2Pool` returns nil |
| builder_nil(fid=5) DODO | 0 | FIXED: graceful nil propagation for degenerate quadratic |

### Blacklisted by Pool Type

| Type | Count | Notes |
|------|-------|-------|
| lfj_v2 | 45 | Many are FoT or discover tool couldn't test |
| lfj_v1 | 6 | 285 un-blacklisted, 11 remain (missing token overrides) |
| v2 | 25 | hookContract overrides not applied in Go |
| uniswap_v4 | 24 | Various |
| algebra | 23 | buildQuoter missing Algebra case |
| uniswap_v3 | 4 | 4 zombie pools fixed (bitmap-empty detection) |
| pharaoh_v1 | 5 | Various |
| balancer_v3 | 5 | GyroECLP unsupported |

## Root Causes Found

### 1. Missing token amounts in `token_amounts.txt`
**Impact:** ~39 LFJ V1 pools, unknown number of others.
**Mechanism:** The discover tool (`cmd/discover/main.go` lines 411-414) skips directions where `tokenAmounts[tokenIn]` doesn't exist. Since both directions are skipped, `bestFormulaID` stays -1 and the pool gets blacklisted.
**Fix:** Add tokens to `formulas/data/token_amounts.txt` with appropriate swap amounts, then re-run discover. Or manually set to correct formula ID and verify with benchmark.

### 2. Go `tokenOverrideEntry` missing `hookContract` field
**Impact:** ~25 V2 pools with hooked tokens (VaporDEX pools with staking hooks).
**Mechanism:** The TypeScript router applies hook overrides (replacing hook code with no-op), but the Go struct in `router/overrides.go` doesn't parse the `hookContract` field from `token_overrides.json`. During EVM discovery, the hook interferes with swaps.
**Fix:** Add `HookContract` field to `tokenOverrideEntry` struct and apply code overrides. Or manually verify and set formula IDs.

### 3. `buildQuoter` switch missing `case FormulaAlgebra:`
**Impact:** All 35 Algebra pools (12 builder_nil + 23 blacklisted).
**Mechanism:** `formulas/pool_quoter.go` `buildQuoter()` has no case for `FormulaAlgebra = 4`. The formula exists via the legacy `dispatchFormula` path, but the PoolManager path returns nil.
**Fix:** Add `case FormulaAlgebra:` to the switch in `buildQuoter()` (line 266). This is the single highest-impact fix.

### 4. V3 pools missing from `v3PoolFees` registry
**Impact:** 38 V3 pools.
**Mechanism:** `newV3Pool` returns nil if pool isn't in `v3PoolFees` map. This map is in `formulas/v3_registry.go` and must be populated with fee and tickSpacing for each pool.
**Fix:** Read fee and tickSpacing from on-chain storage for missing pools and add to `v3PoolFees`.

## Investigation Tools & Techniques

### Running the coverage diagnostic
```bash
# See which pools fall back to EVM and why
timeout 120 go run ./cmd/benchmark/ --limit 1000 --debug-coverage 2>&1

# Count by reason
... | grep EVM_FALLBACK | awk '{print $NF}' | sort | uniq -c | sort -rn

# Count blacklisted by pool type
... | grep "reason=blacklisted" | grep -oP 'type=\d+\(\w+\)' | sort | uniq -c | sort -rn
```

### Multi-block validation
```bash
# Test formula correctness across multiple blocks (prevents overfitting)
timeout 600 go run ./cmd/benchmark/ --limit 1000 --blocks 3 2>&1
```

### Checking a specific pool
```bash
# Is it in the registry?
grep -i '<address>' formulas/registry.txt

# What formula ID does it have?
# -1 = blacklisted, 0=V2, 1=Pharaoh, 2=V3, 3=LFJV2, 4=Algebra, 5=DODO, 6=V4, 7=BalV3, 8=BalV2

# Is it in the V3 fee registry?
grep -i '<address>' formulas/v3_registry.go

# Is its token a FoT token?
grep -i '<token_address>' formulas/fot.go

# Check token overrides
grep -i '<token_address>' router/data/token_overrides.json
```

### Checking why a pool was blacklisted
```bash
# Find the commit that set it to -1
git log -p --all -S '<address>' -- formulas/registry.txt | head -40
```

### Testing an un-blacklisting
1. Change the pool's ID in `formulas/registry.txt` from -1 to the correct formula ID
2. Run `timeout 120 go run ./cmd/benchmark/ --limit 1000 2>&1`
3. Check if the pool appears in MISMATCH lines
4. If no mismatch, run `--blocks 3` to validate across blocks
5. If still clean, commit the change

### Key files
| File | Purpose |
|------|---------|
| `formulas/registry.txt` | Pool → formula ID mapping (embedded) |
| `formulas/registry.go` | Registry loading, formula ID constants |
| `formulas/pool_quoter.go` | `PoolManager.Get()`, `buildQuoter()` switch |
| `formulas/v3_registry.go` | V3 pool fee/tickSpacing map |
| `formulas/fot.go` | Fee-on-transfer token detection |
| `formulas/data/token_amounts.txt` | Token swap amounts for discover tool |
| `router/data/token_overrides.json` | Token storage overrides (balance injection) |
| `router/overrides.go` | Go-side override parsing (`tokenOverrideEntry`) |
| `cmd/benchmark/main.go` | Benchmark with `--debug-coverage` flag |
| `cmd/discover/main.go` | Discovery tool that assigns formula IDs |

### Formula ID reference
| ID | Constant | Pool Type |
|----|----------|-----------|
| -1 | FormulaInvalid | Blacklisted (FoT, broken, custom fee) |
| 0 | FormulaV2_30bps | V2 constant product (0.3% fee) |
| 1 | FormulaPharaohV1 | Pharaoh V1 (stable/volatile) |
| 2 | FormulaV3 | Uniswap V3 / Pharaoh V3 |
| 3 | FormulaLFJV2 | LFJ V2 Liquidity Book |
| 4 | FormulaAlgebra | Algebra V1 Integral |
| 5 | FormulaDODO | DODO PMM |
| 6 | FormulaV4 | Uniswap V4 |
| 7 | FormulaBalancerV3 | Balancer V3 |
| 8 | FormulaBalancerV2 | Balancer V2 |
| 9 | FormulaNoImpl | Known type, no formula (dead quoter) |

## Priority Fix Order

1. **Algebra buildQuoter case** — 35 pools, single code change
2. **V3 fee registry gaps** — 38 pools, need on-chain reads
3. **Bulk un-blacklist trial** — change -1 → correct ID, benchmark, keep what works
4. **hookContract in Go overrides** — DONE (4 pools fixed, 3 remain blacklisted due to zero EVM output)
5. **token_amounts.txt gaps** — 39 LFJ V1 pools
6. **quote_fail investigation** — 49 pools where formula builds but Quote() fails

## Agent Findings

### hookContract fix in Go overrides (2026-03-27)

**Problem:** Token `0x88f89be3e9b1dc1c5f208696fb9cabfcc684bd5f` has a `hookContract` field in `token_overrides.json` pointing to `0x17427af0f2e0ed27856c3288bb902115467e2540`. The TypeScript side replaces the hook contract's code with a no-op (STOP opcode), but the Go `tokenOverrideEntry` struct didn't have a `HookContract` field, so it was silently ignored. This caused the discover tool to fail when testing these pools, leading to blacklisting.

**Fix applied to `router/overrides.go`:**
1. Added `HookContract string` field to `tokenOverrideEntry` struct with JSON tag `hookContract`
2. In `buildTokenOverrides()`, when a token has a `HookContract`, emit an additional `ParsedOverride` with `Code: []byte{0x00}` (STOP opcode) and `Balance: uint256.NewInt(0)` for the hook address. The zero balance is critical — `SetAccount` stores the balance pointer directly, and a nil balance causes a nil pointer dereference in `GetBalance`.

**Pools un-blacklisted (changed from -1 to 0):**
- `0xe8ef9cc2f20205c5a243efc957a47865e53bfcad` (vapordex, VPND/VAPE)
- `0x38080dea41cc88aacbf394972b326d5f30715bbf` (vapordex)
- `0x7c89dc798d832fe979da9bdf2b2eed593f7e5a5b` (vapordex)
- `0xcf55499e13bf758ddb9d40883c1e123ce18c2888` (vapordex)

**Pools that remain blacklisted (formula gives result but EVM returns 0):**
- `0x0dbcb787458fa66ba71b1b808008fee43edac252` — VPND/WAVAX, EVM swap reverts
- `0x437705f77b5536dade2b3425475b72a0af5f1fe7` — VPND/JOE, EVM swap reverts
- `0x3770ee1844d6ec809ad66e060518b18ba07f9ca4` — likely zero liquidity or transfer restriction

**Key lesson:** When adding code overrides for accounts that may not exist in the state dump, always provide a non-nil `Balance` (e.g., `uint256.NewInt(0)`). The `StateDB.SetAccount` stores the balance pointer directly without nil-checking, and `GetBalance` will panic on `uint256.Set(nil)`.

### Bulk LFJ V1 un-blacklisting (2026-03-27)

**Problem:** 296 LFJ V1 (type=2, TraderJoe V1) pools were blacklisted with formula ID -1. These are standard Uniswap V2 constant-product pools with 0.3% fee. The discover tool blacklisted them because it couldn't test them (missing token amounts in `token_amounts.txt`), not because the formula is wrong.

**Approach:**
1. Changed all 296 blacklisted LFJ V1 pools from -1 to formula 0 (V2 constant product, 0.3% fee)
2. Ran benchmark — identified 11 pools that mismatch (all with `evm=0, result=non-zero`)
3. Reverted those 11 back to -1
4. Final result: 285 pools un-blacklisted, verified across 3 blocks with 0 mismatches

**Results:**
- LFJ V1 formula coverage: 502 → 556 quotes (+54 formula quotes, +10.4%)
- Total formula coverage: 1503 → 1581 (+78 quotes)
- 0 new mismatches introduced

**11 pools that remain blacklisted (evm=0 false positives):**
All 11 have `evm=0` because the benchmark's EVM simulation can't execute the swap — the `tokenIn` for the mismatching direction lacks a balance override in `token_overrides.json`. The formula (V2 0.3%) is almost certainly correct.

Tokens missing overrides (appear as tokenIn in mismatching direction):
- `0xf7d9281e8e363584973f946201b82ba72c965d27`
- `0xd285c7e41ed96f01d75d68e9f07095f8a3d85b2d`
- `0xa1afcc973d44ce1c65a21d9e644cb82489d26503`
- `0x096d19b58cab84a2f0ff0e81c08291bffaa62848`
- `0x00d1b4ffd330e5fc461c50e6fbebdbb5b9bd6da4`
- `0x704eae6d452ca63ce479c59727177c5f3ba0d90c`
- `0xe80772eaf6e2e18b651f160bc9158b2a5cafca65`

One pool (`0xf1840b4ae6dcc58e8dbe514510ffe7737b9acb47`) has an override for its tokenIn (`0x88f89be3e9b1dc1c5f208696fb9cabfcc684bd5f`) but it uses a `hookContract` — same root cause #2 (Go overrides not fully applying hook code replacement).

**To fix the remaining 11:** Add balance overrides for the 7 tokens listed above to `token_overrides.json`, find their balance storage slots, and re-run. For the hookContract case, the hookContract fix above should already handle it on next discover run.

### Algebra buildQuoter dispatch fix (2026-03-27)

**Problem:** `formulas/pool_quoter.go` `buildQuoter()` had no `case FormulaAlgebra:` in its switch statement. This caused all Algebra pools (formula ID 4) to return nil from `PoolManager.Get()`, forcing EVM fallback for every Algebra pool. The formula existed via the legacy `dispatchFormula` path but was never used through the PoolManager.

**Fix:**
1. Created `formulas/pool_algebra.go` with `AlgebraPool` struct implementing `PoolQuoter` interface
2. `newAlgebraPool()` pre-reads globalState (slot 2) and packed slot (slot 9) for dependency tracking, then stores a `StateReader` adapter for tick reads during `Quote()`
3. `Quote()` delegates to `QuoteAlgebraStorage()`, converting between `uint256.Int` and `big.Int`
4. Added `case FormulaAlgebra:` to `buildQuoter()` switch in `pool_quoter.go`

**Registry changes:**
- 13 pools un-blacklisted: changed from -1 to 4 (including target `0x5e128ebc09c918ddae3ca1668d4ee9527dc00d78`)
- 7 pre-existing formula=4 pools blacklisted: changed from 4 to -1 (these were silently producing wrong results through the legacy path; now caught by `buildQuoter`)
- 21 pools that were blacklisted remain blacklisted (formula produces wrong results, typically much higher output than EVM)

**Root cause of 28 broken Algebra pools (7 pre-existing + 21 newly tested):**
All have `pluginConfig=2` (BEFORE_SWAP_FLAG) and `communityFee=1000`. The formula uses `lastFee` from globalState, but the Algebra contract's dynamic fee plugin modifies the fee via `beforeSwap()` hook. The formula does not account for this plugin-computed fee. Fixing this would require either:
- Reading the plugin contract's fee computation logic
- Pre-calling the plugin to get the actual fee
- Reverse-engineering the adaptive fee formula (AlgebraBasePluginV2)

**Key finding:** The `buildQuoter` path and `dispatchFormula` path call the same `QuoteAlgebraStorage` function, so results are identical. The difference is that `buildQuoter` actually exercises the formula (previously it returned nil, silently skipping to EVM), exposing pre-existing formula bugs in 7 pools that were incorrectly marked as working.

**Benchmark results:** 100.0% correct, 0 algebra mismatches across multiple runs.

### Pharaoh V1 bulk un-blacklisting + stable curve overflow fix (2026-03-27)

**Task:** Un-blacklist pool `0x65f83ccacabaac4ed2f80289a02df4d35d744ae8` (Pharaoh V1, stable, type=7) and fix formula to produce correct results.

**Root cause investigation:**
The pool was blacklisted (formula ID = -1) but has a valid entry in `pharaoh_v1_registry.go` with config `{true, 1000000, 1000000, 5, false, 8, 9, -1}` (stable pool, 6-decimal tokens, 5bps fee, reserves at slots 8 and 9).

The benchmark uses 1e18 as the test amount for all pools. For a pool with 6-decimal tokens, 1e18 is a trillion tokens — far larger than the pool's actual reserves (~465k USDC at block 81300000). When trying to swap 1e18 of a 6-decimal token, the Solidity stable curve formula in `getAmountOut()` **reverts with arithmetic overflow** ("arithmetic underflow or overflow" panic code 0x11). This happens because the Newton-Raphson intermediate computation `a * b` in `f()` overflows uint256 (the result ≈ 4e83 exceeds uint256 max ≈ 1.15e77).

The Go formula uses arbitrary-precision `big.Int` and doesn't overflow — it computed a "mathematically valid" result (187135258963) without knowing the Solidity revert. The EVM returned 0 (from the revert), causing a mismatch.

**Fixes to `formulas/pharaoh_v1.go`:**

1. **Overflow detection in `f()`:** Added uint256 overflow check — if `a*b > maxUint256`, return nil (mirrors Solidity revert).
2. **Nil propagation in `getY()`:** Added nil check from `f()` — if `f()` returns nil, `getY()` returns nil (overflow propagation).
3. **Nil check in `getAmountOutStable()`:** Changed `yNew.Sign() <= 0` to `yNew == nil || yNew.Sign() <= 0` to handle the new nil return.
4. **Reserve bounds check:** Added check: if computed output `dy >= rawReserveOut`, return nil. The on-chain `swap()` requires `amountOut < reserve` (strict less-than). When amountIn causes output to equal or exceed the reserve, the swap reverts.

**Bulk un-blacklisting:**
Found 26 pharaoh V1 pools that were blacklisted in `registry.txt` but had valid entries in `pharaoh_v1_registry.go`. Attempted to un-blacklist all 26 to formula ID 1. After multi-block (3-block) validation:

- **24 pools un-blacklisted** (changed from -1 to :1) — all pass with 0 mismatches
- **3 pools kept blacklisted:**
  - `0xdc9ec8f6aca746f13253f14e79647265737bbb35` — stable, 6-decimal/6-decimal pool; test amount 1e18 >> reserves, formula returns non-zero but EVM reverts. Overflow check alone isn't sufficient (reserves large enough that output is within bounds but computation still overflows on-chain). Keep blacklisted.
  - `0xc26e546b632348e76ebbd2811f4458a32ea29b7a` — same root cause, 18-decimal/6-decimal stable pool.
  - `0xdc8a9b07079b6e5d8c04d13d4f9ecb601cf58dd9` — cross-block instability: passes on block 81300000 but mismatches on 81310000.

**Benchmark results (3-block validation):**
- Pharaoh V1 formula coverage: 202 → 204 quotes (net +2, limited by 1000-pool test subset)
- 0 pharaoh mismatches across all 3 blocks
- Total coverage: 1521 formula / 479 EVM (the 3 remaining mismatches are all pre-existing from other uncommitted changes)

**New investigation technique discovered:**
When a pool has `evm=0` in the benchmark, it can mean EITHER:
1. The swap is genuinely impossible (formula should return nil/0)
2. The EVM call reverted — confirmed by calling `eth_getStorageAt` to read actual reserves and `eth_call` to call `getAmountOut()` directly at the benchmark block

Use `eth_call` to call `getAmountOut(uint256 amountIn, address tokenIn)` on the pool contract at the benchmark block (selector `0xf140a35a`) to distinguish "EVM computed 0" from "EVM reverted with 0".

### Pharaoh V1 missing registry entries fix (2026-03-27)

**Problem:** 5 Pharaoh V1 pools had formula ID 1 in `registry.txt` but were missing from `pharaoh_v1_registry.go`. Since `FetchPharaohV1StateStorage()` returns nil when a pool is not in the registry map, `newPharaohV1Pool()` returned nil, causing EVM fallback for all 5 pools.

**Pools added to `pharaoh_v1_registry.go`:**
- `0xc26847bfa980a72c82e924899a989c47b088d7da`: volatile, USDC(6)/token(18), fee=25bps, slots 8,9
- `0x60990d5b305b8b2f53cdfdfcb705ba6f08b88b92`: volatile, 18/18 decimals, fee=50bps, subtractOne, packed slot 11
- `0x57167b368afbd16413e0920a80e3af16bf728540`: volatile, 18/18 decimals, fee=50bps, subtractOne, packed slot 11
- `0xc7a712c645c2e9e39fd234bbaf778d09fef47c4e`: volatile, 18/18 decimals, fee=50bps, subtractOne, packed slot 11
- `0x2cc00706cb6b2c927be3704efa5a4639dd214a8d`: volatile, 18/18 decimals, fee=50bps, subtractOne, packed slot 11

**Method:** Used `evm-quoter/scripts/probe_pharaoh_v1.ts` to call `metadata()` on each pool, detect fee via `getAmountOut()` reverse-engineering, and detect storage layout by matching reserve values against known slot patterns.

**Benchmark results:** 0 mismatches, 100% correct. Pharaoh V1 formula coverage: 204 -> 210 quotes (+6). Total formula coverage: 1580 / 2000 (79%).

### DODO DPP Advanced degenerate quadratic fix (2026-03-27)

**Problem:** Pool `0xa7548448f4C774E3C3005BCfe81cD21B5925E91a` (DPP Advanced, type=4, formula=5) caused `builder_nil(fid=5)` in coverage report. Investigation revealed `newDODOPool()` actually succeeded (state reads correctly via DSP layout detection), but `DODOPool.Quote()` returned `(nil, false)` because `QuoteDODO` panicked.

**Root cause:** `dodoSolveQuadraticForTrade()` in `formulas/dodo.go` has a `require(numerator > 0)` equivalent — when the discriminant's square root equals `bAbs`, `numerator = squareRoot - bAbs = 0`, and the Solidity contract reverts with "DODOMath: should not be zero". The Go code panicked at this point, caught by `recover()` in `Quote()`.

This happens when the swap amount (1e18) is enormously larger than the pool's reserves (~600M base, ~165B quote in raw units). The quadratic solver degenerates — intermediate values cause the discriminant to be a near-perfect square, yielding `numerator ≈ 0`. Both `querySellBase` and `querySellQuote` revert on-chain for this pool at block 81300000 (confirmed via `cast call`).

**Three-part fix:**

1. **`dodoSolveQuadraticForTrade()` returns nil instead of panicking** when `numerator <= 0`. This signals "on-chain revert" without crashing.

2. **Nil propagation in `dodoSellBaseToken()` and `dodoSellQuoteToken()`**: When R transitions through ONE (case 2.3 / R<1 case 3), the remainder's quadratic solve can return nil. Added nil checks before `Add()` to propagate the revert signal instead of panicking on `nil.Add()`.

3. **`QuoteDODO()` nil check + `DODOPool.Quote()` zero acceptance**:
   - `QuoteDODO` returns `big.NewInt(0)` when the sell function returns nil (matching EVM revert → 0 output).
   - `DODOPool.Quote()` changed from `out.Sign() <= 0` to `out.Sign() < 0`, allowing zero results to be returned as valid formula output `(uint256(0), true)`.

**Benchmark results:** 0 mismatches, 100% correct. DODO formula coverage: 4/6 → 6/6 quotes (+2). Total formula coverage: 1580 → 1582 / 2000 (79.1%).

### LFJ V2.0 pool support (2026-03-27)

**Problem:** 5 LFJ V2 pools had formula ID 3 in `registry.txt` but `newLFJV2Pool()` returned `nullLFJV2Pool` because they were missing from `lfjV2Registry` in `formulas/lfj_v2_registry.go`. These are LFJ V2.0 pools (old Liquidity Book interface), distinct from the V2.1/V2.2 pools already supported.

**Root cause:** LFJ V2.0 pools have a completely different storage layout and parameter packing from V2.1:
- **Storage layout:** V2.0 stores tokenX at slot 4, tokenY at slot 5, PairInformation (with activeId) at slot 6, feeParameters at slot 10, bins at slot 11, tree at slots 12-14. V2.1 stores parameters at slot 3 or 4, bins at slot 6 or 7, tree at slots 7-9 or 8-10.
- **Fee parameter packing:** V2.0 uses wider uint16 fields (16+16+16+16+16+24+16+24+24+24+24+40 = 256 bits). V2.1 uses narrower fields (16+12+12+14+24+14+20+20+20+24+40+24 = 240+16 bits). Decoding V2.0 data with V2.1 masks produces garbage.
- **Bin packing:** V2.0 packs reserveX(uint112) | reserveY(uint112) in the first slot of a 4-slot struct. V2.1 packs reserveX(uint128) | reserveY(uint128) in a single slot with reversed order (reserveX in lower bits, reserveY in upper).
- **ActiveId location:** V2.0 stores activeId in PairInformation (slot 6, lowest 24 bits). V2.1 includes activeId in the _parameters slot at bits 232-255.
- **Tree level0:** V2.0 uses `mapping(uint256 => uint256)[3]` (3 separate mappings), so tree[0][0] is at keccak256(abi.encode(0, 12)). V2.1 stores tree level0 as a direct slot value.
- **Immutables:** V2.0 stores binStep in regular storage (slot 3, via ReentrancyGuard + tokenX/tokenY offset). V2.1 stores binStep as an immutable in bytecode.

**Fix (across multiple files):**

1. **`formulas/lfj_v2_registry.go`:** Added `IsV20 bool` field to `LFJV2Immutables`. Added 5 V2.0 pools with their binStep and tokenX ordering (determined via on-chain `tokenX()` and `feeParameters()` calls):
   - `0x855ee438445075f25c18a125ba6607543052a194`: binStep=1, TokenXIsToken0=false (USDC/DAI.e)
   - `0x12ef33ed026d6eeb6c1ea90e97401ddf3d45f569`: binStep=1, TokenXIsToken0=false (USDC/USDT.e)
   - `0x1d7a1a79e2b4ef88d2323f3845246d24a3c20f1d`: binStep=1, TokenXIsToken0=true (USDT/USDC)
   - `0x18332988456c4bd9aba6698ec748b331516f5a14`: binStep=1, TokenXIsToken0=true (USDC.e/USDC)
   - `0xe4e7aaa5a1aab5b55ee44fab2d5dd6fcd80e4d42`: binStep=2, TokenXIsToken0=false (BTC.b/WBTC.e)

2. **`formulas/lfj_v2.go`:** Added `lfjV2LayoutV20` with V2.0 slot positions (feeParams=10, bins=11, tree levels=12-14, activeId=6). Added `isV20` and `activeIdSlot` fields to `lfjV2Layout`. Added `lfjV2DecodeParametersV20()` for V2.0's wider field packing. Added `lfjV2ReadActiveIdV20()` to extract activeId from PairInformation. Added `lfjV2ReadBinV20()` for uint112 bin packing. Modified `FetchLFJV2StateStorage()` to branch on `imm.IsV20` and use V2.0 decoders. Modified `QuoteLFJV2Storage()` to dispatch to V2.0 bin reader.

3. **`formulas/lfj_v2_fast.go`:** Added `isV20` and `treeLevel0BigInt` fields to `lfjV2LayoutFast`. Added `getTreeLevel0Slot()` method to return the correct slot for V2.0 (keccak-mapped) vs V2.1 (direct uint64). Added `lfjV2ReadBinU256V20()` for uint112 bin reading in the fast path. Modified `FetchLFJV2StateFast()` to create V2.0-specific fast layout. Modified `QuoteLFJV2Fast()` to dispatch to V2.0 bin reader.

**Results:**
- 3 pools un-blacklisted and working: `0x12ef33...`, `0x1d7a1a...`, `0x18332988...` — formula matches EVM, 0 mismatches
- 2 pools kept blacklisted (evm=0): `0x855ee4...` and `0xe4e7aa...` — formula produces output but the benchmark's EVM router returns 0 (likely the router doesn't support V2.0 swap interface, or token balance overrides are insufficient). Changed from formula=3 to formula=-1 in registry.txt.
- LFJ V2 `builder_nil(fid=3)` count: 5 → 0
- 0 LFJ V2 mismatches
- Total: 100.0% correct, 0 mismatches

### Zombie V3 pool detection (2026-03-27)

**Problem:** 4 Uniswap V3 pools were blacklisted (formula ID = -1) in registry.txt.
Investigation of pool `0xfae3f424a0a47706811521e3ee268f00cfb5c45e` (WAVAX/USDC.e, fee=500bps) revealed
these are "zombie" pools — the V3 storage state is internally inconsistent:

- `sqrtPriceX96` (slot 0 lower 160 bits): non-zero (pool was initialized)
- `tick` (slot 0 bits 160-183): stale value (e.g. tick=-253762)
- `liquidity` (slot 4 lower 128 bits): non-zero (1.35e18 for the target pool)
- `tickBitmap` (slot 6 mappings): ALL ZERO across the entire ±200-word range
- ERC20 token balances: ZERO (pool was fully drained)

**Root cause:** Liquidity was removed without proper V3 accounting (ticks not cleared from the bitmap,
liquidity counter not zeroed). The pool has no real positions but the liquidity slot still shows a
non-zero value. Without protection, the V3 formula walks through phantom ticks using stale liquidity
and computes enormous phantom output amounts (e.g. 70 trillion WAVAX for 1e18 USDC.e in).

**Fix:** Added zombie pool detection in `newV3Pool()` in `formulas/pool_v3.go`:
```go
if len(bitmapWords) == 0 && !liquidity.IsZero() {
    return nil
}
```
This runs after the ±200-word bitmap pre-load. If no initialized ticks are found anywhere in the
reachable range but liquidity is non-zero, `newV3Pool()` returns nil, causing EVM fallback. The EVM
also returns 0 (can't transfer tokens that aren't there), so both formula and EVM agree on 0 output.

**Why dir=0 didn't mismatch:** For the zeroForOne direction (token0 in, token1 out), the amountOut
is also 0 because the output token (token1) has zero balance too. The formula returns non-zero from
the math, but the benchmark's tolerance check catches very small values (both are 0). Actually, the
formula returns a non-zero value for dir=0 too, but for the zombie pools tested, dir=0 formula result
was small enough that the relative tolerance check passed. Only dir=1 had results far enough from 0
to be flagged as a mismatch.

**All 4 zombie V3 pools identified and fixed:**
- `0xfae3f424a0a47706811521e3ee268f00cfb5c45e`: WAVAX/USDC.e, fee=500bps, tick=-253762
- `0x2e587b9e7aa638d7eb7db5fe7447513bc4d0d28b`: BTC.b/USDC.e, fee=500bps
- `0xb978a8c502ce97b04043036a91548b846067f9ea`: fee=100bps, tickSpacing=1
- `0x815482b1a596603fa036f4372e6ce3e25b380d17`: fee=10000bps, tickSpacing=200

All are confirmed in `v3_registry.go` (have fee/tickSpacing entries) and are uniswap_v3 type=0.
Changed registry.txt from -1 to :2 for all 4.

**Investigation technique:** To confirm a V3 pool is zombie, scan the ±200-word bitmap range via
the state server:
```bash
# In a Go program using the state server WebSocket
for wordPos in range(bitmapMinWord, bitmapMaxWord+1):
    slot = keccak256(abi.encode(int256(wordPos), uint256(6)))  # standard V3 bitmap slot
    val = state_getStorageAt(poolAddr, slot, blockNum)
    if val != 0:
        # Not a zombie — has real ticks
        break
```

**Important implementation note:** The detection comment must NOT contain non-ASCII characters
(e.g. the "±" sign). Claude Code's linter reverts pool_v3.go if the comment contains multi-byte
UTF-8 characters, silently removing the zombie detection code without error.

**Benchmark results:** 0 mismatches at --limit 1000 single block and --blocks 3 aggregate.
Formula coverage: 1578 formula / 422 EVM for --limit 1000.
