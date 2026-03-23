import { type Log, decodeAbiParameters } from "viem";
import {
  type PoolProvider,
  type SwapEvent,
  type CachedRPC,
  POOL_TYPE_PHARAOH_V1,
} from "../types.ts";
import { V2_SWAP_TOPIC } from "./v2-swap.ts";

const tokenCache = new Map<string, { token0: string; token1: string }>();
const rejected = new Set<string>();

async function getPoolTokens(
  pool: string,
  cachedRPC: CachedRPC,
): Promise<{ token0: string; token1: string } | null> {
  if (rejected.has(pool)) return null;
  const cached = tokenCache.get(pool);
  if (cached) return cached;

  try {
    // Pharaoh V1 (Solidly-style): probe stable() then metadata()
    const stableResult = await cachedRPC.ethCall(pool, "stable()");
    if (!stableResult || stableResult === "0x") {
      rejected.add(pool);
      return null;
    }

    const metadataResult = await cachedRPC.ethCall(pool, "metadata()");
    if (!metadataResult || metadataResult.length < 450) {
      rejected.add(pool);
      return null;
    }

    // metadata() returns: (uint256 dec0, uint256 dec1, uint256 r0, uint256 r1, bool st, address t0, address t1)
    const data = metadataResult.slice(2);
    const token0 = "0x" + data.slice(5 * 64 + 24, 6 * 64);
    const token1 = "0x" + data.slice(6 * 64 + 24, 7 * 64);

    const tokens = {
      token0: token0.toLowerCase(),
      token1: token1.toLowerCase(),
    };
    tokenCache.set(pool, tokens);
    return tokens;
  } catch {
    rejected.add(pool);
    return null;
  }
}

export const pharaohV1: PoolProvider = {
  name: "pharaoh_v1",
  poolType: POOL_TYPE_PHARAOH_V1,
  topics: [V2_SWAP_TOPIC],

  async processLogs(logs: Log[], cachedRPC: CachedRPC): Promise<SwapEvent[]> {
    const swaps: SwapEvent[] = [];

    const logsByPool = new Map<string, Log[]>();
    for (const log of logs) {
      if (log.topics[0] !== V2_SWAP_TOPIC) continue;
      const pool = log.address.toLowerCase();
      if (!logsByPool.has(pool)) logsByPool.set(pool, []);
      logsByPool.get(pool)!.push(log);
    }

    for (const [pool, poolLogs] of logsByPool) {
      const tokens = await getPoolTokens(pool, cachedRPC);
      if (!tokens) continue;

      for (const log of poolLogs) {
        const [amount0In, amount1In, amount0Out, amount1Out] =
          decodeAbiParameters(
            [
              { type: "uint256", name: "amount0In" },
              { type: "uint256", name: "amount1In" },
              { type: "uint256", name: "amount0Out" },
              { type: "uint256", name: "amount1Out" },
            ],
            log.data,
          );

        let tokenIn: string,
          tokenOut: string,
          amountIn: bigint,
          amountOut: bigint;

        if (amount0In > 0n) {
          tokenIn = tokens.token0;
          tokenOut = tokens.token1;
          amountIn = amount0In;
          amountOut = amount1Out;
        } else {
          tokenIn = tokens.token1;
          tokenOut = tokens.token0;
          amountIn = amount1In;
          amountOut = amount0Out;
        }

        if (amountIn <= 0n || amountOut <= 0n) continue;

        swaps.push({
          pool,
          tokenIn,
          tokenOut,
          amountIn,
          amountOut,
          poolType: POOL_TYPE_PHARAOH_V1,
          blockNumber: Number(log.blockNumber),
          providerName: "pharaoh_v1",
        });
      }
    }

    return swaps;
  },
};
