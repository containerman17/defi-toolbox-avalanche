# Backlog

## Multi-token Balancer V3 pools
- 5 pools affected (3 stable 3-token, 2 GyroECLP)
- `PoolQuoter` interface only has `Quote(amountIn, zeroForOne bool)` — can't express which pair among 3+ tokens
- The math (`StableComputeOutGivenExactIn`) already handles N tokens
- Fix: extend interface with `QuoteMulti(amountIn, tokenIn, tokenOut)` or treat each pair as a virtual pool
- GyroECLP additionally needs 781 lines of ellipse math ported from Solidity

## Uniswap V4 pools with hooks
- ~5 pools with `afterSwapReturnDelta` hooks (ArenaHook dynamic fees)
- Hooks take fees from external contracts (`arenaFeeHelper.getTotalFeePpm`)
- Fundamentally unsupportable by pure formula — would need to track external contract state
- Consider: read the fee from the helper contract at quote time (adds 1 storage read)

## Pharaoh V1 stale factory fees
- ~6 pools with ~0.5-1% fee mismatch
- Fee lives on the factory contract, not in pool storage — mutable via `setPairFee()`
- Registry has hardcoded fee from probe time, goes stale when factory changes fee
- Fix: read fee from factory storage (need factory address per pool + factory storage layout)
- ~306 non-packed Pharaoh V1 pools affected

## Pharaoh V1 missing registry entries
- ~580 pharaoh_v1 pools not in `pharaoh_v1_registry.go`
- Need on-chain probing script to discover storage layout, fee, stable flag, decimals
- Each pool needs: `{stable, decimals0, decimals1, fee, subtractOne, reserve0Slot, reserve1Slot, packedSlot}`
- Original probe script (`evm-quoter/scripts/probe_pharaoh_v1.ts`) was deleted

## LFJ V2.0 support
- ~5 pools with old Liquidity Book interface (V2.0 vs V2.1/V2.2)
- Completely different storage layout: feeParams=10, bins=11, tree=12-14, activeId=6
- Different fee parameter bit packing (uint16 vs uint24)
- Different bin packing (uint112 reserves vs uint128)
- Low priority: V2.0 pools have minimal liquidity

## Wombat / Platypus formula
- 6 pools (3 Wombat, 3 Platypus), no formula implementation
- Wombat uses non-standard stableswap with per-asset coverage ratios
- Need to port: coverage function, haircut/fee model, amplification factor

## Pharaoh V3 gas calibration
- PharaohV2 (upgraded beacon) pools may have lower per-tick overhead than V1
- Current 55K/tick estimate may be too conservative for V2
- Need empirical calibration from on-chain gas traces

## Binary state server dump (zstd)
- Current gob encoding works (27s → 2s) but could be smaller with zstd
- Raw binary + zstd would compress repeated contract bytecodes naturally
- Low priority since gob is already fast enough
