import { type Log, decodeAbiParameters } from "viem";
import {
  type PoolProvider,
  type SwapEvent,
  type CachedRPC,
  type PoolType,
} from "../types.ts";

// UniV3-style Swap event: Swap(address indexed sender, address indexed recipient, int256 amount0, int256 amount1, uint160 sqrtPriceX96, uint128 liquidity, int24 tick)
const V3_SWAP_TOPIC =
  "0xc42079f94a6350d7e6235f29174924f928cc2ac818eb64fed8004e115fbcca67";

export { V3_SWAP_TOPIC };

/** Create a V3-style provider with factory verification via factory()+token0()+token1() */
export function createV3Provider(
  name: string,
  poolType: PoolType,
  factories: Set<string>,
): PoolProvider {
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
      const [factory, token0, token1] = await Promise.all([
        cachedRPC.getAddress(pool, "factory()"),
        cachedRPC.getAddress(pool, "token0()"),
        cachedRPC.getAddress(pool, "token1()"),
      ]);

      if (!factories.has(factory.toLowerCase())) {
        rejected.add(pool);
        return null;
      }

      const tokens = { token0, token1 };
      tokenCache.set(pool, tokens);
      return tokens;
    } catch {
      rejected.add(pool);
      return null;
    }
  }

  return {
    name,
    poolType,
    topics: [V3_SWAP_TOPIC],

    async processLogs(logs: Log[], cachedRPC: CachedRPC): Promise<SwapEvent[]> {
      const swaps: SwapEvent[] = [];

      const logsByPool = new Map<string, Log[]>();
      for (const log of logs) {
        if (log.topics[0] !== V3_SWAP_TOPIC) continue;
        const pool = log.address.toLowerCase();
        if (!logsByPool.has(pool)) logsByPool.set(pool, []);
        logsByPool.get(pool)!.push(log);
      }

      for (const [pool, poolLogs] of logsByPool) {
        const tokens = await getPoolTokens(pool, cachedRPC);
        if (!tokens) continue;

        for (const log of poolLogs) {
          const [amount0, amount1] = decodeAbiParameters(
            [
              { type: "int256", name: "amount0" },
              { type: "int256", name: "amount1" },
              { type: "uint160", name: "sqrtPriceX96" },
              { type: "uint128", name: "liquidity" },
              { type: "int24", name: "tick" },
            ],
            log.data,
          );

          const zeroForOne = amount0 > 0n;
          const tokenIn = zeroForOne ? tokens.token0 : tokens.token1;
          const tokenOut = zeroForOne ? tokens.token1 : tokens.token0;
          const amountIn = zeroForOne ? amount0 : amount1;
          const amountOut = zeroForOne ? -amount1 : -amount0;

          if (amountIn <= 0n || amountOut <= 0n) continue;

          swaps.push({
            pool,
            tokenIn,
            tokenOut,
            amountIn,
            amountOut,
            poolType,
            blockNumber: Number(log.blockNumber),
            providerName: name,
          });
        }
      }

      return swaps;
    },
  };
}
