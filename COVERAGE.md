# Formula Coverage Playbook

Living document for investigating and fixing formula coverage gaps.
Agents investigating coverage should read this first, and append findings/tools below.

## Current State (2026-03-27)

1501 formula / 499 EVM fallback out of 2000 quotes (1000 pools × 2 directions).
**75% formula coverage, 100% correctness.**

### EVM Fallback Breakdown

| Reason | Count | Description |
|--------|-------|-------------|
| blacklisted | 174 | Registry says -1; many are false positives from tooling bugs |
| quote_fail | 49 | Pool builds OK but Quote() returns (nil,false) — zero sqrtPrice, empty liquidity, etc. |
| builder_nil(fid=2) V3 | 38 | Pool not in `v3PoolFees` map (missing fee/tickSpacing) |
| builder_nil(fid=4) Algebra | 12 | `buildQuoter` switch missing `case FormulaAlgebra:` |
| not_in_registry | 10 | Pool types without any formula (wombat, synapse, platypus, trident, balancer_v2) |
| builder_nil(fid=3) LFJ V2 | 5 | `newLFJV2Pool` fails — needs investigation |
| builder_nil(fid=8) Bal V2 | 3 | `newBalancerV2Pool` returns nil |
| builder_nil(fid=7) Bal V3 | 2 | `newBalancerV3Pool` returns nil (GyroECLP pools) |
| builder_nil(fid=1) Pharaoh | 2 | `newPharaohV1Pool` returns nil |
| builder_nil(fid=0) V2 | 2 | `newV2Pool` returns nil |
| builder_nil(fid=5) DODO | 1 | `newDODOPool` returns nil |

### Blacklisted by Pool Type

| Type | Count | Notes |
|------|-------|-------|
| lfj_v2 | 45 | Many are FoT or discover tool couldn't test |
| lfj_v1 | 39 | Same — missing token_amounts entries |
| v2 | 25 | hookContract overrides not applied in Go |
| uniswap_v4 | 24 | Various |
| algebra | 23 | buildQuoter missing Algebra case |
| uniswap_v3 | 8 | Various |
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
4. **hookContract in Go overrides** — 25 V2 pools
5. **token_amounts.txt gaps** — 39 LFJ V1 pools
6. **quote_fail investigation** — 49 pools where formula builds but Quote() fails
