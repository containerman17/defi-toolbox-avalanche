import { type Log, decodeAbiParameters, keccak256, toHex } from "viem";
import {
  type PoolProvider,
  type SwapEvent,
  type CachedRPC,
  type PoolType,
} from "../types.ts";

// V2 Swap event: Swap(address indexed sender, uint256 amount0In, uint256 amount1In, uint256 amount0Out, uint256 amount1Out, address indexed to)
const V2_SWAP_TOPIC = keccak256(
  toHex("Swap(address,uint256,uint256,uint256,uint256,address)"),
);

export { V2_SWAP_TOPIC };

/** Create a V2-style provider with factory verification via factory()+token0()+token1() */
export function createV2Provider(
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
          try {
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
              poolType,
              blockNumber: Number(log.blockNumber),
              providerName: name,
            });
          } catch {
            continue;
          }
        }
      }

      return swaps;
    },
  };
}
