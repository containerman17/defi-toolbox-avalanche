import { type Log, decodeAbiParameters, keccak256, toHex } from "viem";
import {
  type PoolProvider,
  type SwapEvent,
  type CachedRPC,
  POOL_TYPE_SYNAPSE,
} from "../types.ts";

// TokenSwap(address indexed buyer, uint256 tokensSold, uint256 tokensBought, uint128 soldId, uint128 boughtId)
const TOKEN_SWAP_TOPIC = keccak256(
  toHex("TokenSwap(address,uint256,uint256,uint128,uint128)"),
);

// Known Synapse stableswap pools on Avalanche with their token indices
const SYNAPSE_TOKENS: Record<string, string[]> = {
  // nUSD/DAI.e/USDC.e/USDt.e pool
  "0xed2a7edd7413021d440b09d654f3b87712abab66": [
    "0xcfc37a6ab183dd4aed08c204d1c2773c0b1bdf46", // 0: nUSD
    "0xd586e7f844cea2f87f50152665bcbc2c279d8d70", // 1: DAI.e
    "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664", // 2: USDC.e
    "0xc7198437980c041c805a1edcba50c1ce5db95118", // 3: USDt.e
  ],
  // nUSD/USDC/USDt pool
  "0xa196a03653f6cc5ca0282a8bd7ec60e93f620afc": [
    "0xcfc37a6ab183dd4aed08c204d1c2773c0b1bdf46", // 0: nUSD
    "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e", // 1: USDC
    "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7", // 2: USDt
  ],
};

const SYNAPSE_POOLS = new Set(Object.keys(SYNAPSE_TOKENS));

export const synapse: PoolProvider = {
  name: "synapse",
  poolType: POOL_TYPE_SYNAPSE,
  topics: [TOKEN_SWAP_TOPIC],

  async processLogs(logs: Log[], _cachedRPC: CachedRPC): Promise<SwapEvent[]> {
    const swaps: SwapEvent[] = [];
    // Track which pools had swaps and their max block
    const poolsWithSwaps = new Set<string>();
    const poolMaxBlock = new Map<string, number>();

    for (const log of logs) {
      if (log.topics[0] !== TOKEN_SWAP_TOPIC) continue;
      const pool = log.address.toLowerCase();
      if (!SYNAPSE_POOLS.has(pool)) continue;

      try {
        // Decode non-indexed data: tokensSold (uint256), tokensBought (uint256), soldId (uint128), boughtId (uint128)
        const [tokensSold, tokensBought, soldId, boughtId] =
          decodeAbiParameters(
            [
              { type: "uint256", name: "tokensSold" },
              { type: "uint256", name: "tokensBought" },
              { type: "uint128", name: "soldId" },
              { type: "uint128", name: "boughtId" },
            ],
            log.data,
          );

        if (tokensSold <= 0n || tokensBought <= 0n) continue;

        const fromIdx = Number(soldId);
        const toIdx = Number(boughtId);
        const tokens = SYNAPSE_TOKENS[pool];
        if (!tokens || fromIdx >= tokens.length || toIdx >= tokens.length) continue;

        const blockNumber = Number(log.blockNumber);
        poolsWithSwaps.add(pool);
        poolMaxBlock.set(pool, Math.max(poolMaxBlock.get(pool) ?? 0, blockNumber));

        swaps.push({
          pool,
          tokenIn: tokens[fromIdx],
          tokenOut: tokens[toIdx],
          amountIn: tokensSold,
          amountOut: tokensBought,
          poolType: POOL_TYPE_SYNAPSE,
          blockNumber,
          providerName: "synapse",
          extraData: `from=${fromIdx},to=${toIdx}`,
        });
      } catch {
        continue;
      }
    }

    // Emit synthetic events to ensure all token pairs end up in StoredPool.
    // Each directed pair gets its own edge with the appropriate extraData indices.
    for (const pool of poolsWithSwaps) {
      const tokens = SYNAPSE_TOKENS[pool];
      if (!tokens || tokens.length <= 2) continue;

      const blockNumber = poolMaxBlock.get(pool) ?? 0;
      for (let i = 0; i < tokens.length; i++) {
        for (let j = 0; j < tokens.length; j++) {
          if (i === j) continue;
          swaps.push({
            pool,
            tokenIn: tokens[i],
            tokenOut: tokens[j],
            amountIn: 0n,
            amountOut: 0n,
            poolType: POOL_TYPE_SYNAPSE,
            blockNumber,
            providerName: "synapse",
            extraData: `from=${i},to=${j}`,
          });
        }
      }
    }

    return swaps;
  },
};
