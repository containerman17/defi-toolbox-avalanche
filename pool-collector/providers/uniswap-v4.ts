import { type Log, decodeAbiParameters } from "viem";
import {
  type PoolProvider,
  type SwapEvent,
  type CachedRPC,
  type StoredPool,
  POOL_TYPE_UNIV4,
} from "../types.ts";

const UNIV4_POOL_MANAGER =
  "0x06380c0e0912312b5150364b9dc4542ba0dbbc85";

// Swap(bytes32 indexed id, address indexed sender, int128 amount0, int128 amount1, uint160 sqrtPriceX96, uint128 liquidity, int24 tick, uint24 fee)
const V4_SWAP_TOPIC =
  "0x40e9cecb9f5f1f1c5b9c97dec2917b7ee92e57ba5563708daca94dd84ad7112f";

// Initialize(bytes32 indexed id, address indexed currency0, address indexed currency1, uint24 fee, int24 tickSpacing, address hooks)
const V4_INIT_TOPIC =
  "0xdd466e674ea557f56295e2d0218a125ea4b4f0f6f3307b95f85e6110838d6438";

export { V4_INIT_TOPIC, UNIV4_POOL_MANAGER };

interface V4PoolInfo {
  id: string;
  currency0: string;
  currency1: string;
  fee: number;
  tickSpacing: number;
  hooks: string;
}

// Pool ID → token/config mapping, populated by Initialize events
const v4Pools = new Map<string, V4PoolInfo>();

function parseV4ExtraData(extraData?: string): Partial<V4PoolInfo> {
  const parsed: Partial<V4PoolInfo> = {};
  if (!extraData) {
    return parsed;
  }

  for (const part of extraData.split(",")) {
    const [key, value] = part.split("=");
    if (!key || value === undefined) {
      continue;
    }
    if (key === "id") {
      parsed.id = value.toLowerCase();
    } else if (key === "fee") {
      parsed.fee = Number(value);
    } else if (key === "ts") {
      parsed.tickSpacing = Number(value);
    } else if (key === "hooks") {
      parsed.hooks = value.toLowerCase();
    }
  }

  return parsed;
}

function formatV4ExtraData(info: V4PoolInfo): string {
  return `id=${info.id},fee=${info.fee},ts=${info.tickSpacing},hooks=${info.hooks}`;
}

function pseudoAddressFromPoolId(poolId: string): string {
  return ("0x" + poolId.slice(2, 42)).toLowerCase();
}

/** Process Initialize events to build pool ID → token mapping and persist the pool immediately. */
export function processV4InitLog(log: Log): SwapEvent | undefined {
  if (log.address.toLowerCase() !== UNIV4_POOL_MANAGER) return;
  if (!log.topics[1] || !log.topics[2] || !log.topics[3]) return;

  const poolId = log.topics[1].toLowerCase();
  const currency0 = ("0x" + log.topics[2].slice(26)).toLowerCase();
  const currency1 = ("0x" + log.topics[3].slice(26)).toLowerCase();

  // Decode data: uint24 fee, int24 tickSpacing, address hooks
  let fee = 0;
  let tickSpacing = 0;
  let hooks = "0x0000000000000000000000000000000000000000";

  if (log.data && log.data.length >= 2 + 3 * 64) {
    try {
      const [feeVal, tsVal, hooksVal] = decodeAbiParameters(
        [
          { type: "uint24", name: "fee" },
          { type: "int24", name: "tickSpacing" },
          { type: "address", name: "hooks" },
        ],
        log.data as `0x${string}`,
      );
      fee = Number(feeVal);
      tickSpacing = Number(tsVal);
      hooks = (hooksVal as string).toLowerCase();
    } catch {
      // best effort
    }
  }

  const info: V4PoolInfo = {
    id: poolId,
    currency0,
    currency1,
    fee,
    tickSpacing,
    hooks,
  };
  v4Pools.set(poolId, info);

  return {
    pool: pseudoAddressFromPoolId(poolId),
    tokenIn: currency0,
    tokenOut: currency1,
    amountIn: 0n,
    amountOut: 0n,
    poolType: POOL_TYPE_UNIV4,
    blockNumber: Number(log.blockNumber),
    providerName: "uniswap_v4",
    extraData: formatV4ExtraData(info),
  };
}

/** Seed V4 pool map from persisted pool metadata. */
export function seedV4Pools(
  pools: Iterable<StoredPool>,
): { seeded: number; legacyWithoutId: number } {
  let count = 0;
  let legacyWithoutId = 0;
  for (const pool of pools) {
    if (pool.poolType !== POOL_TYPE_UNIV4) continue;
    const parsed = parseV4ExtraData(pool.extraData);
    if (!parsed.id) {
      legacyWithoutId++;
      continue;
    }
    if (!v4Pools.has(parsed.id)) {
      v4Pools.set(parsed.id, {
        id: parsed.id,
        currency0: pool.tokens[0],
        currency1: pool.tokens[1],
        fee: parsed.fee ?? 0,
        tickSpacing: parsed.tickSpacing ?? 0,
        hooks:
          parsed.hooks ?? "0x0000000000000000000000000000000000000000",
      });
      count++;
    }
  }
  return { seeded: count, legacyWithoutId };
}

export const uniswapV4: PoolProvider = {
  name: "uniswap_v4",
  poolType: POOL_TYPE_UNIV4,
  topics: [V4_SWAP_TOPIC, V4_INIT_TOPIC],

  async processLogs(logs: Log[], _cachedRPC: CachedRPC): Promise<SwapEvent[]> {
    const swaps: SwapEvent[] = [];

    for (const log of logs) {
      // Handle Initialize events inline during discovery
      if (log.topics[0] === V4_INIT_TOPIC) {
        const initEvent = processV4InitLog(log);
        if (initEvent) {
          swaps.push(initEvent);
        }
        continue;
      }

      if (log.topics[0] !== V4_SWAP_TOPIC) continue;
      if (log.address.toLowerCase() !== UNIV4_POOL_MANAGER) continue;
      if (!log.topics[1]) continue;

      const poolId = log.topics[1].toLowerCase();
      const info = v4Pools.get(poolId);
      if (!info) continue;

      swaps.push({
        pool: pseudoAddressFromPoolId(poolId),
        tokenIn: info.currency0,
        tokenOut: info.currency1,
        amountIn: 0n, // V4 swap events don't give us directional amounts during discovery
        amountOut: 0n,
        poolType: POOL_TYPE_UNIV4,
        blockNumber: Number(log.blockNumber),
        providerName: "uniswap_v4",
        extraData: formatV4ExtraData(info),
      });
    }

    return swaps;
  },
};
