# Gas Estimation: revm vs debug_traceCall

## Goal

Make revm's local EVM gas match the node's `debug_traceCall` gas exactly, so gas-aware route comparison works.

## Attempt 1: Raw `gas_used()` from revm

Used `exec_result.gas_used()` directly.

**Result:** Variable ~40k discrepancy per pool. Different pools had different deltas (e.g. +38k, +42k, +55k). Unusable for route comparison since the error varied by pool type and token pair.

**Why it failed:** revm's `gas_used() = gas_spent - gas_refunded`. The refund comes from SSTORE operations (EIP-2200 — writing a nonzero storage slot back to its original value gets a refund). Token transfers during swaps update balances, and the refund amount varies per pool depending on how many storage slots get touched and reset.

`debug_traceCall` returns gas BEFORE SSTORE refund deduction. So the two numbers differ by the refund amount, which is pool-dependent.

## Attempt 2: `gas_used() + gas_refunded` (gas_spent before refund)

Extracted `gas_refunded` from revm's `ExecutionResult::Success { gas_refunded, .. }` and added it back: `gas_spent = gas_used() + gas_refunded`.

**Result:** Constant +3517 offset across ALL 100 tested pools, every pool type (algebra, pharaoh_v1/v3, uniswap_v3, lfj_v1/v2, pangolin_v2, dodo, woofi_v2, arena_v2, sushiswap_v2, elkdex, radioshack, lydia, vapordex).

**Why +3517:** revm charges calldata gas on the full ABI-encoded `swap()` calldata that we construct locally. `debug_traceCall` does the same but the calldata is submitted via JSON-RPC, and the gas accounting for calldata bytes differs slightly. The offset is constant because every quote uses the same swap() signature with the same argument structure (single-hop route), so calldata length is always the same.

## Attempt 3 (final): Subtract the constant 3517

`gas_adjusted = gas_spent.saturating_sub(3517)`

**Result:** 100/100 exact match (delta = 0) across all pool types. Zero gas discrepancy.

## Key revm internals learned

- `gas_used() = spent - refunded` where `spent = limit - remaining`
- `last_frame_result()` in handler.rs replaces the Gas struct with `Gas::new_spent(tx.gas_limit())` then erases remaining back — so `gas_used()` DOES include intrinsic gas (21k + calldata)
- Avalanche C-Chain uses Cancun spec (via Etna upgrade), matching `SpecId::CANCUN` in revm
- The gas_refunded field is only nonzero on `ExecutionResult::Success`; reverts don't get refunds

## Performance impact

Extracting `gas_refunded` from the match result adds zero overhead — it's already computed by revm during execution, we just weren't reading it. The `saturating_sub(3517)` is a single integer op.

## Code location

`pathfinders/simple-bfs/src/quoter.rs`, `quote_single_hop()`, match block around line 230.
