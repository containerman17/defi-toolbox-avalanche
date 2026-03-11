# Router Package Refactor Plan

Goal: clean up the router package to match the public API defined in `IDEA.md`.

---

## Phase 1: Delete & Flatten

### Delete files
- [x] `contracts/init_bytecode.hex` — no deployment from this package
- [x] `example/demo.ts` + `example/` dir — will rewrite later

### Flatten `src/` → root
Move all files from `src/` up one level. The `src/` pattern is a tsc-era leftover.

Before:
```
router/
├── src/
│   ├── index.ts
│   ├── pools.ts
│   ├── discovery.ts
│   ├── types.ts
│   ├── cached-rpc.ts
│   └── providers/
```

After:
```
router/
├── index.ts
├── pools.ts
├── discovery.ts
├── types.ts
├── cached-rpc.ts
└── providers/
```

- [x] Move files
- [x] Update all relative imports (`../data/` → `./data/`)
- [x] Update `package.json` main: `"src/index.ts"` → `"index.ts"`
- [x] Update `package.json` files array
- [x] Update `package.json` scripts: `"node src/discovery.ts"` → `"node discovery.ts"`

---

## Phase 2: Clean Up Dead Code

### Remove from `cached-rpc.ts`
- [x] `getDecimals()` — never called by any provider
- [x] `getSymbol()` — never called by any provider

### Remove from `types.ts` CachedRPC interface
- [x] `getDecimals` method
- [x] `getSymbol` method

### Trim `index.ts` exports (internal-only functions)
Remove from public exports:
- [x] `savePools` — internal to `discover`
- [x] `parsePools` — internal to `loadPools`
- [x] `serializePools` — internal to `savePools`
- [x] `mergePools` — internal to `discover`
- [x] `CachedRpcClient` — internal to `discover`
- [x] `providers` — internal to `discover`
- [x] `defaultPoolsPath` — internal to `discover`
- [x] `CachedRPC` type — internal
- [x] `PoolProvider` type — internal
- [x] `SwapEvent` type — internal

Keep exported:
- `discover`
- `loadPools`
- `StoredPool` type
- `PoolType` type
- `POOL_TYPES`
- Individual `POOL_TYPE_*` constants

---

## Phase 3: Fix `discover()` — Auto-Copy Bundled Snapshot

Current behavior: if `poolsPath` doesn't exist, starts from scratch (chain head - 1M blocks).

New behavior: if `poolsPath` doesn't exist, copy bundled `data/pools.txt` from the package first, then scan forward from that snapshot's head block.

Change in `discovery.ts` (the else branch around line 193):
```typescript
// Before:
pools = new Map();
const chainHead = await getBlockNumber(archivalRpcUrl);
startBlock = Math.max(1, chainHead - DEFAULT_START_BACK);

// After:
const bundledPath = path.join(import.meta.dirname, "data/pools.txt");
fs.mkdirSync(path.dirname(poolsPath), { recursive: true });
fs.copyFileSync(bundledPath, poolsPath);
const loaded = loadPools(poolsPath);
headBlock = loaded.headBlock;
pools = loaded.pools;
startBlock = headBlock + 1;
console.error(`Copied bundled snapshot (${pools.size} pools), resuming from block ${startBlock}`);
```

---

## Phase 4: Add New Functions

### 4a. `ROUTER_ADDRESS` constant

Add to `types.ts`:
```typescript
export const ROUTER_ADDRESS = "0xfd201d9b8f03861022803847526443220315bf4c" as const;
```

### 4b. `encodeSwap(route, amountIn)` — new file `encode.ts`

Takes `RouteStep[]` + `amountIn: bigint`, returns hex calldata string.

Encodes: `swap(address[] pools, uint8[] poolTypes, address[] tokens, uint256 amountIn, bytes[] extraDatas)`

Logic:
- For each step, extract pool address, poolType, tokenIn/tokenOut
- For V4 pools (poolType === 9): use PoolManager address instead of pseudo-address, encode full poolId + fee + tickSpacing + hooks into extraData bytes
- For all others: use stored address, empty extraData bytes (`"0x"`)
- Build the `tokens` array: `[step0.tokenIn, step0.tokenOut, step1.tokenOut, ...]`
- ABI encode with viem's `encodeFunctionData`

V4 PoolManager address (constant from Hayabusa.sol):
```
0x06380c80d1e45dbbfc9e77db1a3d7ef52ceab23b
```

V4 extraData bytes (on-chain): `abi.encode(bytes32 poolId, uint24 fee, int24 tickSpacing, address hooks)`
Parsed from `StoredPool.extraData` string: `"id=0x...,fee=3000,ts=1,hooks=0x000..."`

```typescript
export interface RouteStep {
  pool: StoredPool;
  tokenIn: string;
  tokenOut: string;
}

export function encodeSwap(route: RouteStep[], amountIn: bigint): `0x${string}`;
```

### 4c. `getBalanceOverride(token, amount, holder?)` — new file `overrides.ts`

Reads `data/token_overrides.json` (lazy-loaded, cached in memory).

For a standard ERC20 (slot N):
- Balance slot = `keccak256(abi.encode(holder, slot))`
- Returns `{ [token]: { stateDiff: { [slotHex]: amountHex } } }`

For ERC-7201 tokens (`erc7201_base` field):
- Balance slot = `keccak256(abi.encode(holder, erc7201_base))`
- Same return format

`holder` defaults to `ROUTER_ADDRESS` (the deployed router, for quoting).

```typescript
export function getBalanceOverride(
  token: string,
  amount: bigint,
  holder?: string,
): Record<string, { stateDiff: Record<string, string> }>;
```

### 4d. `quoteRoute(client, route, amountIn)` — new file `quote.ts`

1. Call `encodeSwap(route, amountIn)` to get calldata
2. Build state overrides:
   - Give a dummy sender address balance of `route[0].tokenIn`
   - Set allowance from dummy sender → `ROUTER_ADDRESS`
3. Call `client.call({ account: dummySender, to: ROUTER_ADDRESS, data: calldata, stateOverride })`
4. Decode return value as `uint256` → return as `bigint`

```typescript
export async function quoteRoute(
  client: PublicClient,
  route: RouteStep[],
  amountIn: bigint,
): Promise<bigint>;
```

The dummy sender is a fixed address (e.g., `0x000000000000000000000000000000000000dEaD`).

State overrides needed:
- `getBalanceOverride(inputToken, amountIn, dummySender)` — give dummy sender the input tokens
- Allowance override: set `allowance[dummySender][ROUTER_ADDRESS] = maxUint256` on the input token

The allowance slot computation uses `token_overrides.json` too (standard: `keccak256(abi.encode(ROUTER_ADDRESS, keccak256(abi.encode(dummySender, allowanceSlot))))`, or ERC-7201 variant).

So `overrides.ts` should also export:
```typescript
export function getAllowanceOverride(
  token: string,
  owner: string,
  spender: string,
  amount?: bigint,  // defaults to maxUint256
): Record<string, { stateDiff: Record<string, string> }>;
```

---

## Phase 5: Rewrite `index.ts`

Final public API:

```typescript
// Pool management
export { discover } from "./discovery.ts";
export { loadPools } from "./pools.ts";

// Quoting + encoding
export { quoteRoute } from "./quote.ts";
export { encodeSwap, type RouteStep } from "./encode.ts";
export { getBalanceOverride, getAllowanceOverride } from "./overrides.ts";

// Types + constants
export {
  type StoredPool,
  type PoolType,
  POOL_TYPES,
  POOL_TYPE_UNIV3,
  POOL_TYPE_ALGEBRA,
  POOL_TYPE_LFJ_V1,
  POOL_TYPE_LFJ_V2,
  POOL_TYPE_DODO,
  POOL_TYPE_WOOFI,
  POOL_TYPE_BALANCER_V3,
  POOL_TYPE_PHARAOH_V1,
  POOL_TYPE_V2,
  POOL_TYPE_UNIV4,
  ROUTER_ADDRESS,
} from "./types.ts";
```

---

## File inventory after refactor

```
router/
├── package.json
├── index.ts              # public exports only
├── types.ts              # StoredPool, PoolType, POOL_TYPES, ROUTER_ADDRESS
├── pools.ts              # loadPools (+ internal savePools, parsePools, etc.)
├── discovery.ts          # discover() with auto-copy bundled snapshot
├── cached-rpc.ts         # internal RPC cache for discovery
├── encode.ts             # NEW: encodeSwap()
├── overrides.ts          # NEW: getBalanceOverride(), getAllowanceOverride()
├── quote.ts              # NEW: quoteRoute()
├── providers/            # internal, one file per DEX protocol
│   ├── index.ts
│   ├── v3-swap.ts
│   ├── v2-swap.ts
│   ├── lfj-v2.ts
│   ├── dodo.ts
│   ├── woofi.ts
│   ├── balancer-v3.ts
│   ├── pharaoh-v1.ts
│   └── uniswap-v4.ts
├── contracts/
│   ├── Hayabusa.sol      # reference (will become HayabusaRouter.sol)
│   └── bytecode.hex      # deployed bytecode
├── data/
│   ├── pools.txt         # bundled snapshot
│   └── token_overrides.json
└── notes/
    ├── 01_pool_discovery.md
    └── 02_refactor.md    # this file
```
