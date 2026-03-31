# Formula Discovery Tool — Design Document

## Purpose

Fills `formulas/registry.txt` by testing each pool's formula against EVM execution.
The registry maps pool addresses to formula IDs (0-9) or -1 (no working formula).

## Core Principle: Append-Only

The fill process NEVER overwrites existing registry entries. If a pool already has
an entry (any value including -1), it is skipped. This ensures:

- `-1` means "we tried, formula doesn't work" — it sticks until manually removed
- Valid formula IDs are never accidentally downgraded
- Re-running fill only processes NEW pools

To fix a `-1` pool: fix the formula code, manually delete the entry from
`formulas/registry.txt`, then re-run fill.

## Fill Logic

For each pool NOT in registry:

1. **Get candidate formula ID** from `formulaMap` based on pool type
2. **EVM probe**: quote with base amount from `token_amounts.txt`
   - Reverts treated as zero output (no special cases)
3. **Formula probe**: build a quoter via `pm.BuildQuoterForFormulaID(pool, formulaID)`
   - This bypasses the registry (pool isn't registered yet)
   - The quoter is built fresh, reads state, and quotes
4. **Compare**: if formula output == EVM output (including both being zero), candidate matches
5. **Multi-amount verification**: test with 10 amounts (base × 1..10)
   - ALL 10 must match exactly
   - Zero == zero is a valid match
6. **Record**: if all 10 match → formula ID; if any disagree → -1

## Critical: BuildQuoterForFormulaID

The normal `PoolManager.Get()` checks the registry first — if the pool isn't registered,
it returns nil. But during discovery, pools aren't registered yet (that's the point).

`BuildQuoterForFormulaID(pool, formulaID)` bypasses the registry check and builds a
quoter using the given formula ID directly. This is the ONLY correct way to test
candidate formulas during discovery.

**DO NOT** try to temporarily register pools in the registry to make `Get()` work.
The registry is authoritative and should not be mutated during discovery.

## Pool Type → Formula ID Mapping

```
poolType 0 (uniswap_v3, pharaoh_v3) → formulaID 2 (V3)
poolType 1 (algebra)                 → formulaID 4 (Algebra)
poolType 2 (lfj_v1)                  → formulaID 0 (V2)
poolType 3 (lfj_v2)                  → formulaID 3 (LFJ V2)
poolType 4 (dodo)                    → formulaID 5 (DODO)
poolType 6 (balancer_v3)             → formulaID 7 (Balancer V3)
poolType 7 (pharaoh_v1)              → formulaID 1 (Pharaoh V1)
poolType 8 (v2 family)               → formulaID 0 (V2)
poolType 9 (uniswap_v4)              → formulaID 6 (V4)
```

## Token Amounts

`formulas/data/token_amounts.txt` contains ~1471 tokens with amounts equivalent to
~1 AVAX ($20). Used as base amounts for discovery probing. If neither token in a
pool has an entry, the pool cannot be tested and gets -1.

## Verification

The correctness benchmark (`cmd/benchmark --correctness`) serves as the verification
step. After fill, run the benchmark to confirm 100% correctness. Any mismatches
indicate a formula bug — mark the pool -1 and investigate.
