import { type Log, decodeAbiParameters, keccak256, toHex } from "viem";
import {
  type PoolProvider,
  type SwapEvent,
  type CachedRPC,
  POOL_TYPE_LFJ_V2,
} from "../types.ts";

const LFJ_V2_FACTORIES = new Set([
  "0xb43120c4745967fa9b93e79c149e66b0f2d6fe0c", // V2.2
  "0x8e42f2f4101563bf679975178e880fd87d3efd4e", // V2.1
  "0x6e77932a92582f504ff6c4bdbcef7da6c198aeef", // V2.0
]);

// LB V2.0/V2.1: Swap(address indexed sender, address indexed to, uint24 id, bytes32 amountsIn, bytes32 amountsOut)
const LB_SWAP_TOPIC_OLD = keccak256(
  toHex("Swap(address,address,uint24,bytes32,bytes32)"),
);
// LB V2.2: Swap(address indexed sender, address indexed to, uint24 id, bytes32 amountsIn, bytes32 amountsOut, uint24 volatilityAccumulator, bytes32 totalFees, bytes32 protocolFees)
const LB_SWAP_TOPIC_V22 = keccak256(
  toHex(
    "Swap(address,address,uint24,bytes32,bytes32,uint24,bytes32,bytes32)",
  ),
);
// LB V2.0 minimal-proxy pools: Swap(address indexed sender, address indexed recipient, uint256 indexed activeId, bool swapForY, uint256 amountIn, uint256 amountOut, uint256 volatilityAccumulator, uint256 fees)
const LB_SWAP_TOPIC_V20_PROXY = keccak256(
  toHex("Swap(address,address,uint256,bool,uint256,uint256,uint256,uint256)"),
);

const tokenCache = new Map<string, { tokenX: string; tokenY: string }>();
const rejected = new Set<string>();

async function getPoolTokens(
  pool: string,
  cachedRPC: CachedRPC,
): Promise<{ tokenX: string; tokenY: string } | null> {
  if (rejected.has(pool)) return null;
  const cached = tokenCache.get(pool);
  if (cached) return cached;

  try {
    // V2.1+ use getFactory()/getTokenX()/getTokenY(); V2.0 proxies use factory()/tokenX()/tokenY()
    let factory: string, tokenX: string, tokenY: string;
    try {
      [factory, tokenX, tokenY] = await Promise.all([
        cachedRPC.getAddress(pool, "getFactory()"),
        cachedRPC.getAddress(pool, "getTokenX()"),
        cachedRPC.getAddress(pool, "getTokenY()"),
      ]);
    } catch {
      [factory, tokenX, tokenY] = await Promise.all([
        cachedRPC.getAddress(pool, "factory()"),
        cachedRPC.getAddress(pool, "tokenX()"),
        cachedRPC.getAddress(pool, "tokenY()"),
      ]);
    }

    if (!LFJ_V2_FACTORIES.has(factory)) {
      rejected.add(pool);
      return null;
    }

    const tokens = { tokenX, tokenY };
    tokenCache.set(pool, tokens);
    return tokens;
  } catch {
    rejected.add(pool);
    return null;
  }
}

function decodePackedAmounts(packed: `0x${string}`): {
  amountX: bigint;
  amountY: bigint;
} {
  const value = BigInt(packed);
  const amountX = value & ((1n << 128n) - 1n);
  const amountY = value >> 128n;
  return { amountX, amountY };
}

export const lfjV2: PoolProvider = {
  name: "lfj_v2",
  poolType: POOL_TYPE_LFJ_V2,
  topics: [LB_SWAP_TOPIC_OLD, LB_SWAP_TOPIC_V22, LB_SWAP_TOPIC_V20_PROXY],

  async processLogs(logs: Log[], cachedRPC: CachedRPC): Promise<SwapEvent[]> {
    const swaps: SwapEvent[] = [];

    const logsByPool = new Map<string, Log[]>();
    for (const log of logs) {
      const topic = log.topics[0];
      if (topic !== LB_SWAP_TOPIC_OLD && topic !== LB_SWAP_TOPIC_V22 && topic !== LB_SWAP_TOPIC_V20_PROXY) continue;
      const pool = log.address.toLowerCase();
      if (!logsByPool.has(pool)) logsByPool.set(pool, []);
      logsByPool.get(pool)!.push(log);
    }

    for (const [pool, poolLogs] of logsByPool) {
      const tokens = await getPoolTokens(pool, cachedRPC);
      if (!tokens) continue;

      for (const log of poolLogs) {
        const topic = log.topics[0];

        let tokenIn: string,
          tokenOut: string,
          amountIn: bigint,
          amountOut: bigint;

        if (topic === LB_SWAP_TOPIC_V20_PROXY) {
          // Swap(address,address,uint256,bool,uint256,uint256,uint256,uint256)
          // data: swapForY (bool), amountIn, amountOut, volatilityAccumulator, fees
          const [swapForY, amtIn, amtOut] = decodeAbiParameters(
            [
              { type: "bool", name: "swapForY" },
              { type: "uint256", name: "amountIn" },
              { type: "uint256", name: "amountOut" },
              { type: "uint256", name: "volatilityAccumulator" },
              { type: "uint256", name: "fees" },
            ],
            log.data,
          );
          // swapForY=false → tokenY in, tokenX out; swapForY=true → tokenX in, tokenY out
          if (!swapForY) {
            tokenIn = tokens.tokenY;
            tokenOut = tokens.tokenX;
          } else {
            tokenIn = tokens.tokenX;
            tokenOut = tokens.tokenY;
          }
          amountIn = amtIn as bigint;
          amountOut = amtOut as bigint;
        } else {
          const [, amountsIn, amountsOut] = decodeAbiParameters(
            [
              { type: "uint24", name: "id" },
              { type: "bytes32", name: "amountsIn" },
              { type: "bytes32", name: "amountsOut" },
            ],
            log.data,
          );

          const inAmounts = decodePackedAmounts(amountsIn as `0x${string}`);
          const outAmounts = decodePackedAmounts(amountsOut as `0x${string}`);

          if (inAmounts.amountX > 0n) {
            tokenIn = tokens.tokenX;
            tokenOut = tokens.tokenY;
            amountIn = inAmounts.amountX;
            amountOut = outAmounts.amountY;
          } else {
            tokenIn = tokens.tokenY;
            tokenOut = tokens.tokenX;
            amountIn = inAmounts.amountY;
            amountOut = outAmounts.amountX;
          }
        }

        if (amountIn <= 0n || amountOut <= 0n) continue;

        swaps.push({
          pool,
          tokenIn,
          tokenOut,
          amountIn,
          amountOut,
          poolType: POOL_TYPE_LFJ_V2,
          blockNumber: Number(log.blockNumber),
          providerName: "lfj_v2",
        });
      }
    }

    return swaps;
  },
};
