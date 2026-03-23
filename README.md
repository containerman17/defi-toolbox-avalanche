# defi-toolbox-avalanche

A full-stack DEX routing toolkit for Avalanche C-Chain. Covers pool discovery, swap execution, EVM-level quoting, and BFS pathfinding — everything needed to find and execute optimal swap routes.

## Architecture

```
BFS Pathfinding (arbs / optimal swaps)
  |
  |-- Formula Quoter         single-pool math (40x faster than EVM)
  |-- Go EVM Layer           multi-pool execution (WASM + native)
  |-- Router                 on-chain swap contract + quoting
  |-- Pool Collector         on-chain pool discovery engine
  |-- State Server           chain state for off-chain execution
```

### Components

| Component | Location | Description | Benchmarks |
|-----------|----------|-------------|------------|
| Pool Collector | `pool-collector/` | Discovers 26,000+ pools across 35+ protocols | — |
| Router | `router/` | Solidity contract (21 pool types) + TypeScript quoting | Backrun LFG swaps (~99% match) |
| State Server | `tools/state-proxy/` | WebSocket RPC proxy with per-block state caching | — |
| State Dumper | `tools/state-dumper/` | Full contract storage dumper via `debug_storageRangeAt` | — |
| Go EVM Layer | external | EVM execution compiled to WASM and native Go | Speed per block, pool coverage |
| Formula Quoter | external | Direct math for single-pool quotes | Correctness (must be 100%), speed |
| BFS Pathfinding | planned | Graph search over all pool edges | Quality (max output), speed |

## Structure

```
pool-collector/           On-chain pool discovery engine
  providers/              Per-protocol log parsers (35 providers)
  data/pools.txt          Pool catalog (26,000+ pools)
  scripts/update.ts       Entry point: scan chain for new pools

router/                   Solidity contract + TypeScript quoting
  contracts/              HayabusaRouter.sol bytecode
  data/                   Token storage slot overrides (320+ tokens)
  scripts/                Entry points
  benchmarks/             Route analyzer (backrun benchmark)

examples/                 Debug scripts and one-off tests

tools/
  state-proxy/            Go — WebSocket RPC proxy with state caching
  state-dumper/           Go — Full storage dump tool

rpc/                      WebSocket connection pool (viem transport)
utils/                    Shared utilities (.env loader)
mcp/                      MCP server for Routescan API
```

## Supported Protocols

### AMMs (Concentrated Liquidity)

| Protocol | Type |
|----------|------|
| Uniswap V3 | Concentrated liquidity |
| Uniswap V4 | Singleton pool manager |
| Pharaoh V3 | Concentrated liquidity (Algebra-style fees) |
| Blackhole CL | Concentrated liquidity (Algebra-fork) |
| Algebra | Dynamic-fee concentrated liquidity |

### AMMs (Constant Product)

| Protocol | Type |
|----------|------|
| LFJ V1 (Trader Joe) | x*y=k |
| LFJ V2 (Trader Joe) | Liquidity Book (bin-based) |
| Pangolin V2 | x*y=k |
| SushiSwap V2 | x*y=k |
| Pharaoh V1 | x*y=k (Solidly-fork) |
| Blackhole Volatile | x*y=k (Solidly-fork) |
| Uniswap V2 | x*y=k |
| Arena V2 | x*y=k (memecoins) |
| Fraxswap, Swapsicle, Canary, Complus, Lydia, Hurricane, Thorus, RadioShack, VaporDEX, ElkDEX, YetiSwap, PartySwap, OliveSwap, HakuSwap, 0x | x*y=k |

### Stableswaps / Oracle / Vault

| Protocol | Type |
|----------|------|
| Wombat, Platypus | Coverage ratio stableswap |
| Synapse | StableSwap (Saddle-fork) |
| WooFi V2 / WooPP | Oracle-based |
| DODO | Proactive market maker |
| Cavalre | Multiswap |
| KyberSwap DMM | Dynamic market maker |
| Balancer V2 / V3 | Weighted / stable pools |
| Balancer V3 Buffered | Wrap/unwrap through ERC-4626 |
| Trident | BentoBox-backed (SushiSwap) |
| TransferFrom | RFQ / vault pull (Hashflow-style) |

**21 pool types across 35+ protocol deployments, 26,000+ pools cataloged.**

## Quick Start

```bash
npm install

# Discover/update pools (uses public RPC by default)
node pool-collector/scripts/update.ts

# Quote 0.1 WAVAX -> USDC across matching pools
node examples/02_quote_pools/index.ts
```

Custom RPC:

```bash
echo "RPC_URL=http://localhost:9650/ext/bc/C/rpc" > .env
```

## API

```typescript
import { quoteRoute, quoteFlat, ROUTER_ADDRESS } from "./router/index.ts";
import { loadPools, discover } from "./pool-collector/index.ts";

// Load pool catalog
const { pools } = loadPools();

// Quote a multi-hop route
const amountOut = await quoteRoute(client, [
  { pool: pool1, tokenIn: WAVAX, tokenOut: USDT },
  { pool: pool2, tokenIn: USDT, tokenOut: USDC },
], amountIn);

// Quote a flat/DAG route (splits, merges, parallel paths)
const amountOut = await quoteFlat(client, steps, tokenOut);
```

## Status

Raw alpha from an Avalanche ecosystem contributor. The contract has **not been security audited** — use for quoting and research, not production routing. Published as-is with no warranty.
