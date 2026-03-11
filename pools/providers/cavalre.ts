import { type Log, decodeAbiParameters } from "viem";
import {
  type PoolProvider,
  type SwapEvent,
  POOL_TYPE_CAVALRE,
} from "../types.ts";

const CAVALRE_POOLS = new Set([
  "0x5f1e8ed8468232bab71eda9f4598bda3161f48ea",
]);

// Known tokens in the Cavalre Multiswap pool
const CAVALRE_TOKENS: Record<string, string[]> = {
  "0x5f1e8ed8468232bab71eda9f4598bda3161f48ea": [
    "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7", // WAVAX
    "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e", // USDC
    "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7", // USDt
    "0xc891eb4cbdeff6e073e859e987815ed1505c2acd", // EUROC
    "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664", // USDC.e
    "0xc7198437980c041c805a1edcba50c1ce5db95118", // USDT.e
    "0xd586e7f844cea2f87f50152665bcbc2c279d8d70", // DAI.e
    "0x49d5c2bdffac6ce2bfdb6640f4f80f226bc10bab", // WETH.e
    "0x50b7545627a5162f82a992c33b87adc75187b218", // WBTC.e
    "0x152b9d0fdc40c096757f570a51e494bd4b943e50", // BTC.b
  ],
};

// Swap(uint256 indexed txCount, address indexed user, address payToken, address receiveToken, uint256 payAmount, uint256 receiveAmount, uint256 feeAmount)
const SWAP_TOPIC =
  "0x5303f139d7aacabb0b5c8741d56c117c63c6ee5ba97a9d1c50cb09c423c26c2f";

export const cavalre: PoolProvider = {
  name: "cavalre",
  poolType: POOL_TYPE_CAVALRE,
  topics: [SWAP_TOPIC],

  async processLogs(logs: Log[], cachedRPC: CachedRPC): Promise<SwapEvent[]> {
    const swaps: SwapEvent[] = [];

    // Track which pools we've already fetched tokens for
    const poolTokensCache = new Map<string, string[]>();
    // Track which pools had at least one swap in this batch
    const poolsWithSwaps = new Set<string>();
    // Track max block number per pool for synthetic events
    const poolMaxBlock = new Map<string, number>();

    for (const log of logs) {
      const addr = log.address.toLowerCase();
      if (!CAVALRE_POOLS.has(addr)) continue;
      if (log.topics[0] !== SWAP_TOPIC) continue;

      // Decode non-indexed data: payToken, receiveToken, payAmount, receiveAmount, feeAmount
      try {
        const [payToken, receiveToken, payAmount, receiveAmount] =
          decodeAbiParameters(
            [
              { type: "address", name: "payToken" },
              { type: "address", name: "receiveToken" },
              { type: "uint256", name: "payAmount" },
              { type: "uint256", name: "receiveAmount" },
              { type: "uint256", name: "feeAmount" },
            ],
            log.data,
          );

        if (payAmount <= 0n || receiveAmount <= 0n) continue;

        // Get all pool tokens from hardcoded list (assets() returns structs, not addresses)
        if (!poolTokensCache.has(addr)) {
          poolTokensCache.set(addr, CAVALRE_TOKENS[addr] ?? [
            (payToken as string).toLowerCase(),
            (receiveToken as string).toLowerCase(),
          ]);
        }

        const blockNumber = Number(log.blockNumber);
        poolsWithSwaps.add(addr);
        poolMaxBlock.set(
          addr,
          Math.max(poolMaxBlock.get(addr) ?? 0, blockNumber),
        );

        swaps.push({
          pool: addr,
          tokenIn: (payToken as string).toLowerCase(),
          tokenOut: (receiveToken as string).toLowerCase(),
          amountIn: payAmount,
          amountOut: receiveAmount,
          poolType: POOL_TYPE_CAVALRE,
          blockNumber,
          providerName: "cavalre",
        });
      } catch {
        continue;
      }
    }

    // Emit synthetic events to ensure all tokens from assets() end up in the StoredPool.
    // swapEventsToPoolUpdates accumulates tokens from tokenIn/tokenOut of all events
    // for a given pool, so we pair up consecutive tokens from the full token list.
    for (const addr of poolsWithSwaps) {
      const allTokens = poolTokensCache.get(addr);
      if (!allTokens || allTokens.length <= 2) continue;

      const blockNumber = poolMaxBlock.get(addr) ?? 0;
      // Emit events pairing token[0] with each other token to ensure all are stored
      const baseToken = allTokens[0];
      for (let i = 1; i < allTokens.length; i++) {
        swaps.push({
          pool: addr,
          tokenIn: baseToken,
          tokenOut: allTokens[i],
          amountIn: 0n,
          amountOut: 0n,
          poolType: POOL_TYPE_CAVALRE,
          blockNumber,
          providerName: "cavalre",
        });
      }
    }

    return swaps;
  },
};
