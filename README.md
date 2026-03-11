# defi-toolbox-avalanche

A DEX router has three parts: a **swap contract**, a **pool list**, and a **pathfinder**. This repo includes the first two:

- [`HayabusaRouter.sol`](HayabusaRouter.sol) — Solidity contract that swaps through 21 pool types on Avalanche C-Chain in a single atomic call. Uniswap V2/V3/V4, LFJ V1/V2, Algebra, Blackhole, Balancer V2/V3, DODO, WooFi, Wombat, Platypus, Synapse, Trident, and more.
- [`pools/data/pools.txt`](pools/data/pools.txt) — Catalog of 26,000+ active pools discovered on-chain, with a discovery engine to keep it updated.

The pathfinder (route search algorithm) is left to you — plug in your own BFS, split-route optimizer, or whatever fits your use case. The contract and pool list give you everything needed to execute and simulate any route you find.

Deploy the contract on-chain for real swaps, or inject it via `eth_call` code overrides for stateless quoting without deployment.

## Supported Protocols

### AMMs (Concentrated Liquidity)

| Protocol | Type |
|----------|------|
| Uniswap V3 | Concentrated liquidity |
| Uniswap V4 | Singleton pool manager |
| Pharaoh V3 | Concentrated liquidity (Algebra-style fees) |
| [Blackhole](https://blackhole.xyz/) CL | Concentrated liquidity (Algebra-fork) |
| Algebra | Dynamic-fee concentrated liquidity |

### AMMs (Constant Product)

| Protocol | Type |
|----------|------|
| LFJ V1 (Trader Joe) | x*y=k |
| LFJ V2 (Trader Joe) | Liquidity Book (bin-based) |
| Pangolin V2 | x*y=k |
| SushiSwap V2 | x*y=k |
| Pharaoh V1 | x*y=k (Solidly-fork) |
| [Blackhole](https://blackhole.xyz/) Volatile | x*y=k (Solidly-fork) |
| Uniswap V2 | x*y=k |
| Arena V2 | x*y=k (memecoins) |
| Fraxswap | x*y=k |
| Swapsicle | x*y=k |
| Canary | x*y=k |
| Complus | x*y=k |
| Lydia | x*y=k |
| Hurricane | x*y=k |
| Thorus | x*y=k |
| RadioShack | x*y=k |
| VaporDEX | x*y=k |
| ElkDEX | x*y=k |
| YetiSwap | x*y=k |
| PartySwap | x*y=k |
| OliveSwap | x*y=k |
| HakuSwap | x*y=k |
| 0x (ZeroEx) | x*y=k |

### Stableswaps

| Protocol | Type |
|----------|------|
| Wombat | Coverage ratio stableswap |
| Platypus | Coverage ratio stableswap |
| Synapse | StableSwap (Saddle-fork) |

### Oracle / RFQ

| Protocol | Type |
|----------|------|
| WooFi V2 | Oracle-based (ABA protocol) |
| WooPP | Oracle-based pool |
| DODO | Proactive market maker |
| Cavalre | Multiswap |
| KyberSwap DMM | Dynamic market maker |

### Vault / Weighted

| Protocol | Type |
|----------|------|
| Balancer V2 | Weighted / stable pools |
| Balancer V3 | Weighted / stable pools |
| Balancer V3 Buffered | Wrap/unwrap through ERC-4626 |
| ERC-4626 Vaults | Aave V3 wrapped aTokens |
| Trident | BentoBox-backed (SushiSwap) |

### Other

| Protocol | Type |
|----------|------|
| TransferFrom | RFQ / vault pull (Hashflow-style) |

**Total: 21 pool types across 35+ protocol deployments, with 26,000+ pools cataloged.**

## Structure

```
router/           Solidity contract + TypeScript quoting helpers
  contracts/      HayabusaRouter.sol source and compiled bytecode
  data/           Token storage slot overrides (320+ tokens)
pools/            On-chain pool discovery engine
  providers/      Per-protocol log parsers (35 providers)
  data/           Pool catalog (pools.txt)
rpc/              WebSocket connection pool transport (viem-compatible)
utils/            Shared utilities (.env loader)
examples/
  01_update_pools Pool discovery — scan chain for new pools
  02_quote_pools  Quote WAVAX/USDC across all matching pools
```

## Quick Start

```bash
# Install dependencies
npm install

# Discover pools (uses public RPC by default)
node examples/01_update_pools/index.ts

# Quote 0.1 WAVAX → USDC across all WAVAX/USDC pools
node examples/02_quote_pools/index.ts
```

To use a local node or custom RPC:

```bash
echo "RPC_URL=http://localhost:9650/ext/bc/C/rpc" > .env
```

## API

```typescript
import { quoteRoute, quoteFlat, ROUTER_ADDRESS } from "./router/index.ts";
import { loadPools, discover } from "./pools/index.ts";

// Load pool catalog
const { pools } = loadPools();

// Quote a single-hop swap
const amountOut = await quoteRoute(client, [
  { pool, tokenIn: WAVAX, tokenOut: USDC }
], amountIn);

// Quote a multi-hop route
const amountOut = await quoteRoute(client, [
  { pool: pool1, tokenIn: WAVAX, tokenOut: USDT },
  { pool: pool2, tokenIn: USDT, tokenOut: USDC },
], amountIn);

// Quote a flat/DAG route (splits, merges, parallel paths)
const amountOut = await quoteFlat(client, steps, tokenOut);
```

## Status

This is a raw alpha from an Avalanche ecosystem contributor. The contract has **not been security audited** — use it for quoting and research, not for routing real funds in production. The code is published as-is with no warranty. This is a personal project and does not represent an official Ava Labs product.
