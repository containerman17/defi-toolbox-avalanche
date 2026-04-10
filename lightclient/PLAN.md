# Light Client — Implementation Plan

Working document. Updated as modules get implemented and bugs get fixed.

## Architecture Overview

```
┌─────────────────────────────────────────────┐
│                  LightClient                │
│  - Owns the block loop                      │
│  - Coordinates RPC pool, state, execution   │
│  - Exposes: LatestBlock(), Call()            │
├─────────────────┬───────────────────────────┤
│   Block Loop    │      EVM Executor         │
│  - Subscribe    │  - Takes resolved block#  │
│  - Fetch block  │  - Builds vm.BlockContext  │
│  - Execute      │  - Runs txs sequentially  │
│  - Update state │  - Returns diffs          │
├─────────────────┴───────────────────────────┤
│              Versioned State                 │
│  - Per-key linked list by block number      │
│  - Atomic "latest block" pointer            │
│  - Lock-free reads after block resolution   │
│  - vm.StateDB interface for EVM             │
├─────────────────────────────────────────────┤
│              RPC Pool                       │
│  - N blocking worker goroutines             │
│  - Shared work queue (channel)              │
│  - Natural backpressure from socket count   │
├─────────────────────────────────────────────┤
│              Snapshots                      │
│  - Serialize versioned state (latest only)  │
│  - Atomic write (tmp + rename)              │
│  - Load on startup                          │
└─────────────────────────────────────────────┘
```

## Current Status (2026-04-10)

**Verification: 5000/5000 blocks matched. 50,000-block run in progress, 3200+ blocks matched so far, zero errors.**

### Performance (from 50k run, 16-core machine, local node)

| Metric | Value |
|--------|-------|
| **exec p50** | **18ms** |
| **exec p90** | **65ms** |
| exec p95 | 85ms |
| exec p99 | 209ms |
| exec p100 | 499ms |
| fetch p50 | 2ms |
| trace p50 | 66ms (verification only, not hot path) |

p90 drops as cache warms. Cold start has more RPC misses. Steady-state p50 ~18ms is well within 1-second block time. p90 at 65ms is acceptable.

### Bugs Found & Fixed

1. **Trace parser bogus zeros** — prestateTracer `post.Balance == nil` means "not modified," not "went to zero." Removed balance/nonce/code zero-interpretation from pre-only entries. Storage slot deletions (pre-only) are correct.

2. **Dirty tracker vs overlay** — `DirtyStorage`/`DirtyBalances` don't undo on revert. Reverted tx writes leaked into the block diff. Fix: extract diffs from the overlay (`StorageOverrides()`) instead of dirty tracker. Overlay correctly reflects final state after all txs including reverts.

3. **GetCommittedState returning start-of-block** — SSTORE gas/refund calculations (EIP-2200/EIP-3529) use `GetCommittedState` to check the "original" value. We were returning the pre-block state instead of per-tx committed state. Fix: `CommitTx()` snapshots the overlay between transactions. The committed storage map is checked first in `GetCommittedState`. This was causing exactly 2800 gas difference (= `SstoreClearsScheduleRefundEIP3529`) per affected tx.

4. **Platform-level state changes** — P-Chain staking rewards and atomic tx nonce changes are invisible to EVM execution. They modify balances/nonces between blocks outside of any mechanism we can trace. Fix: when `ApplyMessage` fails with "insufficient funds" or "nonce too high," re-fetch the sender's balance and nonce from RPC at `blockNum-1` and retry once. This handles rewards, atomic exports, and any other platform-level state mutation.

5. **New contract storage in trace** — prestateTracer reports zeros for storage of newly created contracts (didn't exist in pre-state). Fix: skip trace comparison for addresses that appear in the code diff (= new deployments). Our execution correctly initializes their storage.

6. **Exist() incomplete** — didn't check `createdAccounts` or overlay maps. An account created in tx N would appear non-existent in tx N+1, potentially causing wrong gas charges for CALLs.

### Trace Limitations Discovered

The prestateTracer with diffMode has several blind spots:
- Doesn't capture balance changes from atomic transactions
- Doesn't capture platform-level balance credits (staking rewards)
- Reports zeros for newly created contract storage
- Not reliable for balance comparison in general

Storage and nonce comparison against the trace is reliable. Balance verification requires chain-state RPC queries.

## Modules

All modules implemented and working.

| Module | File | Lines | Status |
|--------|------|-------|--------|
| Versioned State | state.go | ~1050 | Done |
| RPC Pool | rpc.go | ~400 | Done |
| Block Fetcher | fetch.go | ~460 | Done |
| EVM Executor | executor.go | ~220 | Done |
| Snapshots | snapshot.go | ~170 | Done |
| LightClient | client.go | ~450 | Done |
| **Total** | | **~2750** | |

### Commands

| Command | Purpose |
|---------|---------|
| `cmd/verify/` | Execute blocks, compare storage/nonces vs trace. Running percentile stats every 5s. |
| `cmd/snapshot-bench/` | Test snapshot save/load pipeline. Exit 1 if >100ms on second run. |
| `cmd/debug-gas/` | Per-tx gas comparison: local vs chain receipt. |

## Key Design Decisions

### Overlay-based diffs (not dirty tracker)
The dirty tracker (`dirtyStorage` etc.) doesn't undo on revert. The overlay (`storageOverrides` etc.) does — it's the journaled state. Block diffs are extracted from the overlay.

### CommitTx between transactions
`GetCommittedState` must return per-tx committed state, not per-block. `CommitTx()` snapshots the overlay after each tx. Without this, SSTORE refund calculations produce wrong gas.

### Platform state retry
Avalanche has P-Chain staking rewards and atomic tx nonces that modify C-Chain state outside EVM execution. These are invisible to us. Instead of trying to predict them, we detect failures ("insufficient funds", "nonce too high") and re-fetch from RPC.

### New contract trace skip
Newly deployed contracts have zero storage in the prestateTracer output. We skip trace comparison for addresses with code changes in the diff.

## Open Questions

- Pruning strategy: currently block-count-based. Time-based might be more intuitive.
- Snapshot compression: plain gob works, gob+zstd would reduce disk. Not urgent.
- Reorgs: Avalanche has finality, so not needed for mainnet. Might matter for L1s.
- Prefetch optimization: execute tx 2+ against node state in parallel while locally executing tx 1, to prime cache. Would reduce p90 significantly.

## Future Work

- Lock-free reads via append-only linked list (currently RWMutex for block updates)
- Late-arriving cache miss handling (insert at correct block in linked list)
- Memory pruning for long-running processes (keep last N blocks only)
- Snapshot compression (gob+zstd)
- Prefetch optimization for reducing p90 exec time
