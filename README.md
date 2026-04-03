# defi-toolbox-avalanche

A full-stack DEX routing toolkit for Avalanche C-Chain. Covers pool discovery, formula-based quoting, EVM execution, BFS pathfinding, and on-chain arbitrage — everything needed to find and execute optimal swap routes.

## Architecture

```
BFS Pathfinding (arbs / optimal swaps)
  |
  |-- Formula Quoter         single-pool math (formulas/)
  |-- Go EVM Layer           multi-pool execution, WASM + native (statedb/)
  |-- Router                 on-chain swap contract (contracts/)
  |-- Pool Collector         on-chain pool discovery (tools/pool-collector/)
  |-- State Server           live chain state via WebSocket (cmd/state-server/)
```

### Components

| Component | Location | Description | Performance |
|-----------|----------|-------------|-------------|
| Formula Quoter | `formulas/` | Direct math for 10 pool types, 98.8% accuracy | 44ms / 2000 pools (native) |
| State DB | `statedb/` | In-memory EVM state with per-block diffs | Gob wire format, 2s WASM connect |
| State Server | `cmd/state-server/` | WebSocket server broadcasting per-block state diffs | ~900K storage entries |
| Pool Collector | `tools/pool-collector/` | Discovers 26,000+ pools across 35+ protocols | — |
| Router | `contracts/` | HayabusaRouter.sol (21 pool types) | On-chain at `0x476f...` |
| BFS Pathfinder | `pathfinder/` | Graph search over all pool edges | — |
| WASM SDK | `cmd/wasm-sdk/` | Browser-ready WASM quoter + eth_call | 115ms / 2000 pools |
| Quoter API | `quoter/` | Quote orchestration (BFS + formulas + EVM) | — |

## Structure

```
cmd/
  state-server/             WebSocket state server (gob initial dump + JSON diffs)
  wasm-sdk/                 Browser WASM SDK (quoter + eth_call)

quoter/                     Quote API (BFS pathfinding + formula engine + EVM verification)

examples/
  go/
    arbitrage/              WAVAX arbitrage bot with on-chain execution
    http-quoter/            HTTP quote server
  browser/
    01_eth_call/            In-browser EVM vs public RPC latency comparison
    02_live_quotes/         Live round-trip spread table

formulas/                   Formula-based pool quoters (10 types)
  pool_quoter.go            PoolManager, quote cache, dead pool dirs
  pool_v2.go                V2 constant product (0.3% fee)
  pool_v3.go                V3 concentrated liquidity (Uniswap/Pharaoh)
  pool_v4.go                V4 singleton pool manager
  pool_lfj_v2.go            LFJ V2 Liquidity Book (V2.0 + V2.1)
  pool_algebra.go           Algebra V1 Integral
  pool_dodo.go              DODO PMM
  pool_pharaoh_v1.go        Pharaoh V1 (Solidly-fork)
  pool_balancer_v2.go       Balancer V2 weighted/stable
  pool_balancer_v3.go       Balancer V3 (2-token)
  fot.go                    Fee-on-transfer token handling
  registry.txt              Pool → formula ID mapping (7,900+ pools)
  data/token_amounts.txt    Realistic ~$1 swap amounts per token

statedb/                    In-memory EVM state
  livestate.go              Live state with block subscriptions
  immutable.go              Storage/account maps with in-place diff
  callstate.go              EVM call context (StateDB interface)
  wire/dump.go              Gob wire format (zero external deps)

contracts/                  On-chain router
  HayabusaRouter.sol        Solidity source (21 pool types)
  bytecode.hex              Compiled bytecode
  overrides.go              Token balance/approval state overrides
  token_overrides.json      Per-token storage slot configs (320+ tokens)

pathfinder/                 BFS graph search
  bfs.go                    Multi-hop path finding
  pools.go                  Pool graph construction
  encode.go                 Route encoding for router

tools/
  pool-collector/           Pool discovery engine (TypeScript, 15 providers)
  token-pricer/             EVM-based token price discovery at deploy block
  discover/                 Pool type auto-detection

benchmarks/
  formula-accuracy/         Formula vs EVM correctness (98.8%, 2000 pools)
  swap-replay/              Historical swap replay testing

experiments/                Archived prototypes (arb1, arb2, arb3)
mcp/                        MCP server for Routescan API
```

## Supported Protocols

### AMMs (Concentrated Liquidity)

| Protocol | Formula | Type |
|----------|---------|------|
| Uniswap V3 | FormulaV3 | Concentrated liquidity |
| Uniswap V4 | FormulaV4 | Singleton pool manager (+ ArenaHook) |
| Pharaoh V3 | FormulaV3 | Concentrated liquidity (dynamic fees) |
| Blackhole CL | FormulaAlgebra | Concentrated liquidity (dynamic fees) |
| Algebra | FormulaAlgebra | Dynamic-fee concentrated liquidity |

### AMMs (Constant Product / Bin-based)

| Protocol | Formula | Type |
|----------|---------|------|
| LFJ V1 (Trader Joe) | FormulaV2 | x*y=k |
| LFJ V2 (Trader Joe) | FormulaLFJV2 | Liquidity Book (V2.0 + V2.1) |
| Pangolin V2 | FormulaV2 | x*y=k |
| SushiSwap V2 | FormulaV2 | x*y=k |
| Pharaoh V1 | FormulaPharaohV1 | x*y=k (dynamic factory fees) |
| Blackhole Volatile | FormulaPharaohV1 | x*y=k |
| Uniswap V2 | FormulaV2 | x*y=k |
| Arena V2 | FormulaV2 | x*y=k (memecoins) |
| Fraxswap, Swapsicle, Canary, Complus, Lydia, Hurricane, Thorus, RadioShack, VaporDEX, ElkDEX, YetiSwap, PartySwap, OliveSwap, HakuSwap, 0x | FormulaV2 | x*y=k |

### Stableswaps / Oracle / Vault

| Protocol | Formula | Type |
|----------|---------|------|
| DODO | FormulaDODO | Proactive market maker |
| Balancer V2 | FormulaBalancerV2 | Weighted / stable pools |
| Balancer V3 | FormulaBalancerV3 | Weighted / stable (2-token) |
| Balancer V3 Buffered | — | Wrap/unwrap through ERC-4626 |
| Wombat, Platypus | — | Coverage ratio stableswap (spec ready) |
| Synapse | — | StableSwap |
| WooFi V2 / WooPP | — | Oracle-based |
| Cavalre | — | Multiswap |
| KyberSwap DMM | — | Dynamic market maker |
| Trident | — | BentoBox-backed |
| TransferFrom | — | RFQ / vault pull |

**10 formula types, 21 pool types, 35+ protocol deployments, 26,600+ pools cataloged, 7,900+ with formula quoters.**

## Quick Start

Requires a local Avalanche C-Chain node at `http://localhost:9650/ext/bc/C/rpc`.

```bash
# Run formula accuracy benchmark (2000 pools)
timeout 300 go run ./benchmarks/formula-accuracy/ --limit 2000 2>&1

# Run arbitrage example
go run ./examples/go/arbitrage/ --pool-limit 200

# Start state server
go run ./cmd/state-server/

# Update pool catalog (TypeScript)
npm install
node tools/pool-collector/scripts/update.ts
```

## Performance

| Metric | Value |
|--------|-------|
| Formula accuracy | 98.8% (3929/3975 tested, 2000 pools) |
| Native quote speed | 44ms / 2000 pools |
| WASM quote speed | 115ms / 2000 pools |
| WASM connect time | ~2s (gob wire format) |
| Pool catalog | 26,600+ pools |
| Formula coverage | 7,900+ pools with quoters |

## Status

Raw alpha from an Avalanche ecosystem contributor. The contract has **not been security audited** — use for quoting and research, not production routing. Published as-is with no warranty.
