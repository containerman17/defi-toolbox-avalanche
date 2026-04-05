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

## State server: use debug_storageRangeAt instead of eth_getStorageAt

### Problem
- Cold startup fetches ~700K keys one-by-one via `eth_getStorageAt` → 20 minutes
- `eth_getStorageAt` returns `0x0` for both zero-valued and non-existent slots
- Attackers can query non-existent slots, filling cache with zeros that get shipped to all clients in `initial_dump`
- State server must run next to node for latency

### Solution: debug_storageRangeAt
- `debug_storageRangeAt(block, txIndex, contractAddress, keyStart, maxResult)` iterates the **actual trie**
- Only returns slots that exist (non-zero) — absent slots are simply not returned
- Supports pagination via `nextKey` in response
- Tested on mainnet USDC (0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E):
  - 1,000 slots: 2.6s, 159 KB response
  - 10,000 slots: 22.7s, 1.6 MB response
  - ~2.3ms per slot (walks trie, not snapshot — slow for huge contracts like USDC 3.8M slots)
  - For small DeFi pools (~50 slots): ~100ms per contract, very fast

### Benefits
1. **Cold start**: bulk fetch whole contracts in one call, not key-by-key
2. **Zero vs absent solved**: only real non-zero slots returned
3. **No cache pollution**: attacker queries for non-existent slots can be rejected
4. **Clean initial dump**: only contains slots that actually exist

### Implementation notes
- On startup: call `debug_storageRangeAt` for each tracked contract with large `maxResult`
- On block diff: if a diff sets slot to zero, **delete** from cache instead of storing zero
- On client cache miss: check if slot exists before fetching (or use bloom filter, see below)
- Caveat: Firewood backend returns `errFirewoodNotSupported` — breaks if node migrates to Firewood

### Client-side: bloom filters for absent key detection
- State server builds bloom filter from all known non-zero slot hashes
- Ship bloom filter alongside `initial_dump` (~120 KB for 100K slots at 1% FPR)
- Client checks bloom on cache miss: **no** = definitely zero (instant), **yes** = maybe exists (fetch)
- Eliminates nearly all garbage fetches from exploratory EVM execution
- Update strategy: rebuild bloom on each block (cheap) or use counting bloom filter

## Full C-chain state snapshot research (2026-04-03)

Explored extracting full state from AvalancheGo's PebbleDB. Key findings:

### Database structure
- All data in single PebbleDB at `~/.avalanchego-mainnet/db/mainnet/pebble/` (~600 GB)
- Keys are nested SHA256 prefixes: `SHA256(SHA256(chainID) + "vm") + SHA256("ethdb") + evmKey`
- C-chain VM accounts for 445 GB of the 600 GB total

### C-chain EVM breakdown
| Category | Disk size | Description |
|---|---|---|
| Snapshot accounts (`a` prefix) | 5 GB | 84M accounts (flat keccak256(addr) → RLP) |
| Snapshot storage (`o` prefix) | 34.5 GB | 756M storage slots (keccak256(addr) + keccak256(slot) → RLP value) |
| Contract code (`c` prefix) | 2.6 GB | 7M code entries |
| Block bodies (`b`) | 27.7 GB | Historical transactions |
| Receipts (`r`) | 18.4 GB | Historical receipts |
| Trie nodes (`A`, `O`, `L`) | 4.1 GB | Path-based merkle trie (pruned) |
| Headers, tx lookup, bloom | 14.8 GB | Indexing data |
| Unknown (0x00 prefix) | ~337 GB | Likely old hash-based trie nodes |

### Full state dump
- Dumped all snapshot accounts + storage + code to binary file
- 848M entries, 67 GB raw, **34 GB zstd compressed**
- Poor compression (2x) because keys are keccak256 hashes (random bytes)
- Account hash (32 bytes) repeats per contract → compresses well
- Storage hash (32 bytes) unique per slot → incompressible
- USDC alone: 3.8M slots, 514 MB raw, ~167 MB on disk

### Conclusion
- Light execution node using full state in RAM (~60 GB) is feasible but not clearly better than state server architecture
- For DeFi use case, state server next to node using `debug_storageRangeAt` is the practical path
- Full state dump useful as research artifact, not as production approach
