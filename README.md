# DeFi Toolbox — Avalanche C-Chain

A Go library for DEX quoting on Avalanche C-Chain. Formula-based pool math, BFS pathfinding across 2,000+ pools, and a full EVM that runs in the browser via WebAssembly.

## What it does

Given a token pair and amount, finds the best swap route across every major DEX on Avalanche in ~50ms. Works two ways:

- **Go library** — import `quoter/`, `formulas/`, `pathfinder/`, `statedb/` into your own Go code
- **Browser SDK** — a WASM binary (`cmd/wasm-sdk/`) that runs the same engine in a browser tab, synced to live chain state via WebSocket

## Products

| | Location | What |
|---|---|---|
| **State Server** | `cmd/state-server/` | WebSocket server that streams per-block state diffs. Clients get a full copy of relevant chain state on connect, then incremental updates. |
| **WASM SDK** | `cmd/wasm-sdk/` | Go→WASM binary exposing `quote()` and `ethCall()` to JavaScript. Runs a full EVM in the browser. |

## Library

| Package | What |
|---|---|
| `quoter/` | Quote API — takes token pair + amount, returns best route with BFS pathfinding + EVM verification |
| `formulas/` | Single-pool math for 10 pool types (V2, V3, V4, LFJ V2, Algebra, DODO, Balancer, etc). 98.8% accuracy vs on-chain |
| `pathfinder/` | BFS graph search over pool edges, multi-hop route finding |
| `statedb/` | In-memory EVM state with per-block diffs, WebSocket sync, gob wire format |
| `contracts/` | HayabusaRouter — on-chain swap router supporting 21 pool types |

## Examples

| | Location | What |
|---|---|---|
| In-browser EVM | `examples/browser/01_eth_call/` | Latency comparison: local WASM `eth_call` vs public RPC, per block |
| Live spreads | `examples/browser/02_live_quotes/` | Round-trip spread table for 5 tokens, continuous quoting |
| Arbitrage bot | `examples/go/arbitrage/` | WAVAX arbitrage with EVM verification and on-chain execution |
| HTTP quoter | `examples/go/http-quoter/` | HTTP server wrapping the quoter API |

## Quick Start

Requires a local Avalanche C-Chain node with WebSocket at `ws://localhost:9650/ext/bc/C/ws`.

```bash
# Start state server
go run ./cmd/state-server/

# Build WASM SDK
make build-wasm

# Run arbitrage example
go run ./examples/go/arbitrage/ --pool-limit 200

# Run formula accuracy benchmark
go run ./benchmarks/formula-accuracy/ --limit 2000
```

## Performance

| Metric | Value |
|--------|-------|
| Quote speed (native) | 44ms / 2000 pools |
| Quote speed (WASM) | 115ms / 2000 pools |
| Formula accuracy | 98.8% vs on-chain EVM |
| WASM connect time | ~2s |
| Pool catalog | 26,600+ pools |
| Formula coverage | 7,900+ pools |

## Supported DEXes

**Concentrated liquidity:** Uniswap V3/V4, Pharaoh V3, Algebra, Blackhole CL

**Constant product:** Trader Joe V1, Pangolin, SushiSwap, Uniswap V2, Pharaoh V1, Arena, and 15+ smaller forks

**Liquidity Book:** Trader Joe V2 (V2.0 + V2.1/V2.2)

**Stableswap / other:** DODO, Balancer V2/V3, Wombat, Platypus, Synapse, WooFi, KyberSwap, Cavalre

10 formula types, 21 pool types, 35+ protocol deployments.

## Status

Raw alpha. The router contract has **not been security audited** — use for quoting and research, not production routing.
