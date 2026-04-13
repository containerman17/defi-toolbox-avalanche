# Achieving EVM Quoting Parity with the LFJ Meta-Router

## Why LFJ

LFJ (Trader Joe) operates a **meta-router** on Avalanche C-Chain. It's not a single DEX — it aggregates across every major pool type on the chain: UniV2, UniV3, UniV4, LFJ V1/V2, Algebra, Pharaoh, Balancer V3, WooPP, DODO, Wombat, Platypus, Synapse, Trident, KyberDMM, and more. When a user swaps through LFJ, the router splits the trade across multiple paths through multiple pool types to minimize slippage.

Achieving quoting parity with LFJ means our router can reproduce the output of any route LFJ constructs. This is the gold standard because:

1. **Complete pool coverage** — if LFJ routes through a pool, we need to be able to quote it
2. **Split-route accuracy** — LFJ splits trades across 2-44 parallel paths; we need to simulate the same sequential pool-state mutations
3. **Arbitrary token support** — LFJ handles hundreds of tokens with different ERC-20 implementations; our simulation must handle all of them

Current benchmark: **14818/14968 PASS (99.0%)** across 15,000 real LFJ swap transactions.

## The Pipeline

```
find_txs.ts → txs.txt → convert.ts → payloads/*.json → test.ts
```

### Step 1: Collect Transactions

`find_txs.ts` scans Avalanche blocks for `LFJSwap` events emitted by the LFJ router. It outputs tx hashes to `txs.txt`.

```bash
node examples/03_route_analyzer/find_txs.ts [startBlock] [count]
```

### Step 2: Reconstruct Routes

`convert.ts` replays each transaction at `block - 1` via `debug_traceCall`, extracts Transfer events, and reconstructs the swap route — which pools were used, in what order, with what amounts.

```bash
WS_URL=ws://localhost:8545 node examples/03_route_analyzer/convert.ts
```

The output is a JSON payload per transaction containing: input/output tokens, amounts, pool addresses, pool types, step ordering for split routes.

### Step 3: Quote and Compare

`test.ts` loads each payload, calls `quoteRoute()` or `quoteFlat()` (eth_call with state overrides at `block - 1`), and compares the simulated output against the actual on-chain output.

```bash
WS_URL=ws://localhost:8545 node examples/03_route_analyzer/test.ts
```

## The Hard Part: Token Balance Overrides

This is where 90% of the work went. The problem is deceptively simple.

### The Problem

To simulate a swap via `eth_call`, we inject a fake token balance into the router contract using `stateOverride`. But ERC-20 tokens store balances in a Solidity mapping (`mapping(address => uint256)`), and the **storage slot of that mapping varies per contract**.

To override a balance, you need to know: which slot the `_balances` mapping lives at, then compute `keccak256(abi.encode(address, slot))` to get the actual storage key.

### The Variety

| Pattern | Slot | Tokens |
|---------|------|--------|
| Standard OpenZeppelin | 0 | ~100 tokens |
| USDC (FiatTokenV2) | 9 | USDC |
| USDt (TetherToken) | 51 | USDt, bridged variants |
| Aave aTokens | 52 | aAvaUSDC, aAvaUSDT, etc. |
| BridgeToken proxy | 5 | WETH.e, WBTC.e, etc. |
| Diamond proxy | 394, 516 | Specialized tokens |
| ERC-7201 namespaced | hash-based | OFT tokens, newer upgradeable |
| Packed struct (USDV) | 208, shift:32 | Balance stored as uint64 at bit offset 32 |
| Reflection tokens | slot 2 + rOwned/rTotal/tTotal | Tax/redistribution tokens |
| Custom allowance slot | varies | MIM (allowance at slot 16, not slot 3) |

### Special Cases

**USDV** — Balance is stored as a `uint64` inside a packed struct at slot 208. The value occupies bits 32-95 of the 256-bit storage word. Override requires `shift: 32`.

**COQ (BIFKN314)** — Has a `maxWallet` transfer restriction. Even with the correct balance override, transfers revert with `MaxWalletAmountExceeded`. Fix: zero out storage slot 16 (`maxWalletEnabled`) via `disableSlots: [16]`.

**ERC-7201 tokens** — Use a namespaced storage base: `keccak256("openzeppelin.storage.ERC20") - 1 = 0x52c63247...20bace00`. Balance mapping is at that base, allowance at base+1.

### The Override File

`router/data/token_overrides.json` — array of entries:

```json
{
  "address": "0x...",
  "slot": 208,
  "shift": 32,
  "disableSlots": [16],
  "erc7201_base": "0x52c63247...",
  "erc7201_allowance": "0x52c63247...",
  "allowance_slot": 16
}
```

Currently 170+ entries. Without an override, `getBalanceOverride()` returns nothing, the `eth_call` gets zero balance, the swap reverts, and the route appears to produce zero output.

### Finding Storage Slots

`examples/find_slots_batch.ts` automates slot discovery:

1. Find a holder with nonzero balance (from Transfer logs or known addresses)
2. Call `balanceOf(holder)` to get the expected value
3. For each candidate slot (0-20, 51-52, 101, 151, 208, ...), compute `keccak256(abi.encode(holder, slot))` for both Solidity and Vyper ordering
4. Call `eth_getStorageAt` at that computed key
5. If the stored value matches the balance, that's the slot

## Split Route Simulation

### The Problem

LFJ splits a single trade across multiple parallel paths. For example, swapping 1000 USDC to WAVAX might split into:
- 400 USDC → algebra → WAVAX
- 350 USDC → uniswap_v3 → WAVAX
- 250 USDC → pharaoh_v3 → WAVAX

Simple enough. But it gets complex when:
- Paths share pools (path A and path B both use the same UniV3 pool)
- Paths produce intermediate tokens (path A: USDC→USDt, path B: USDt→WAVAX)
- Paths have diamond patterns (input splits and reconverges)

### Flat Quoting

The primary approach: pack all steps into a single `quoteFlat()` call. This sends one `eth_call` where pool state carries across all steps, naturally handling shared pools.

### Retry Strategies

When flat quoting fails or gives bad results, `test.ts` tries:

1. **Proportional redistribution** — Scale each step's amountIn so the sum equals totalAmountIn (handles rounding gaps from convert.ts)
2. **Topological sort** — Reorder steps so intermediate-producing legs execute before intermediate-consuming legs
3. **Per-step fallback** — Quote each step independently (loses shared-pool accuracy but handles edge cases)
4. **Greedy flat** — Permute step ordering to find the best result

### Tolerance

Two tolerance systems account for simulation-vs-reality divergence:

**Single routes:** `5000 + (hops-1) * 15000 + oracleHops * 5000` ppm
- 1-hop: 0.5%
- 2-hop: 2.0%
- 3-hop: 3.5%

**Split routes:** `basePct + ceil(poolReuseRatio * 50)`%
- 1-2 steps: 2% base
- 3-4 steps: 5% base
- 5-8 steps: 8% base
- 9+ steps: 20% base
- Plus up to 50% more for heavily-shared pools

## Pool Types

| ID | Name | Notes |
|----|------|-------|
| 0 | uniswap_v2 | Standard x*y=k (SushiSwap, etc.) |
| 1 | lfj_v1 | Trader Joe V1 |
| 2 | lfj_v2_1 | Trader Joe V2.1 |
| 3 | lfj_v2 | Trader Joe V2 |
| 4 | dodo | DODO PMM (proactive market maker) |
| 5 | woopp | WooPP V1 oracle-based |
| 6 | balancer_v3 | Balancer V3 weighted/stable |
| 7 | uniswap_v3 | Concentrated liquidity |
| 8 | algebra | Dynamic-fee UniV3 (Pharaoh V3) |
| 9 | uniswap_v4 | Singleton pool manager |
| 10 | erc4626 | Vault wrap/unwrap (yield-bearing) |
| 11 | balancer_v3_buffered | BalV3 with ERC4626 buffer |
| 12 | wombat | Wombat stableswap (coverage ratio) |
| 13 | platypus | Platypus stableswap |
| 14 | woopp_v2 | WooPP V2 (ABA protocol) |
| 15 | transfer_from | RFQ/vault pull (Hashflow-style) |
| 17 | arena_v2 | Arena bonding curve (memecoins) |
| 18 | kyber_dmm | KyberSwap DMM |
| 19 | synapse | Synapse StableSwap |
| 20 | trident | Trident (BentoBox-backed) |
| 21 | pharaoh_v1 | Pharaoh V1 (UniV2-style) |
| 22 | pangolin_v2 | Pangolin V2 |

## Infrastructure

### WebSocket Cache Proxy

`~/experiments-private/2026/03/04_ws_cache_proxy/` — a Go proxy that:
- Accepts WebSocket connections from clients
- Maintains a pool of N blocking WebSocket connections to the upstream node (N = nproc)
- Caches deterministic RPC responses (eth_call with specific block, getTransactionReceipt, etc.) to disk
- Passes through non-cacheable requests (eth_blockNumber, latest queries)
- Auto-reconnects upstream connections on failure

```bash
# Start in tmux
tmux new-session -d -s ws-proxy \
  "./ws-cache-proxy -listen :8545 -upstream ws://localhost:9650/ext/bc/C/ws \
   -cache-dir /path/to/cache"
```

Point `WS_URL=ws://localhost:8545` to route through the proxy. The 15k-tx benchmark runs in ~2 minutes with a warm cache vs 15+ minutes without.

### WebSocket Pool Transport

`rpc/ws-pool.ts` — a viem-compatible transport that maintains multiple WebSocket connections with blocking semantics (one request per connection at a time). This naturally limits concurrency and prevents overwhelming the node.

## Remaining Failures (1%)

| Category | Count | Fixable? |
|----------|-------|----------|
| Large shared-pool deviation | 60 | No — pool state diverges fundamentally when simulating paths in different order than on-chain |
| Dead/illiquid pools (-100%) | 26 | No — pool has near-zero reserves at block-1 |
| KyberSwap forwarding (no swap events) | 25 | No — aggregator routes through uncataloged bonding curves |
| Missing pools/bridges | 14 | Partially — some need pool types we don't support (Curve meta-pools) |
| Contract reverts | 13 | No — broken/dead memecoin tokens |
| Suspicious (>2x expected) | 12 | No — heavily shared pools give inflated results |

## Key Files

| File | Purpose |
|------|---------|
| `examples/03_route_analyzer/find_txs.ts` | Scan blocks for LFJ swap events |
| `examples/03_route_analyzer/convert.ts` | Replay txs, reconstruct routes, generate payloads |
| `examples/03_route_analyzer/test.ts` | Quote-and-compare benchmark harness |
| `examples/03_route_analyzer/txs.txt` | 15,000 tx hashes |
| `examples/find_slots_batch.ts` | Batch storage slot discovery for ERC-20 tokens |
| `router/overrides.ts` | Balance/allowance override injection logic |
| `router/data/token_overrides.json` | 170+ token storage slot entries |
| `router/quote.ts` | `quoteRoute()` and `quoteFlat()` — eth_call wrappers |
| `pools/data/pools.txt` | Pool catalog (25k+ pools) |
| `rpc/ws-pool.ts` | WebSocket connection pool transport |

## Quick Reference

```bash
# Run benchmark (through proxy)
WS_URL=ws://localhost:8545 node examples/03_route_analyzer/test.ts

# Regenerate payloads for new txs (skips existing)
WS_URL=ws://localhost:8545 node examples/03_route_analyzer/convert.ts

# Find balance storage slots for new tokens
# Edit TOKENS array in examples/find_slots_batch.ts, then:
node examples/find_slots_batch.ts

# Quick pool rescan (subtract 100k from line 1 of pools.txt, then)
node examples/01_update_pools/index.ts

# Check if a token has an override
grep -i "TOKEN_PREFIX" router/data/token_overrides.json

# Check if a pool is in the catalog
grep -i "POOL_PREFIX" pools/data/pools.txt

# Start the cache proxy
tmux new-session -d -s ws-proxy \
  "cd ~/experiments-private/2026/03/04_ws_cache_proxy && \
   ./ws-cache-proxy -listen :8545 \
   -upstream ws://localhost:9650/ext/bc/C/ws \
   -cache-dir ~/defi-toolbox-avalanche/cache/rpc"
```
