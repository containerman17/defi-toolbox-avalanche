import { type Log, decodeAbiParameters, keccak256, toHex } from "viem";
import {
  type PoolProvider,
  type SwapEvent,
  type CachedRPC,
  POOL_TYPE_DODO,
} from "../types.ts";

// DODOSwap(address fromToken, address toToken, uint256 fromAmount, uint256 toAmount, address trader, address receiver)
const DODO_SWAP_TOPIC = keccak256(
  toHex("DODOSwap(address,address,uint256,uint256,address,address)"),
);

export const dodo: PoolProvider = {
  name: "dodo",
  poolType: POOL_TYPE_DODO,
  topics: [DODO_SWAP_TOPIC],

  async processLogs(logs: Log[], _cachedRPC: CachedRPC): Promise<SwapEvent[]> {
    const swaps: SwapEvent[] = [];

    for (const log of logs) {
      if (log.topics[0] !== DODO_SWAP_TOPIC) continue;

      const pool = log.address.toLowerCase();
      const [fromToken, toToken, fromAmount, toAmount] = decodeAbiParameters(
        [
          { type: "address", name: "fromToken" },
          { type: "address", name: "toToken" },
          { type: "uint256", name: "fromAmount" },
          { type: "uint256", name: "toAmount" },
          { type: "address", name: "trader" },
          { type: "address", name: "receiver" },
        ],
        log.data,
      );

      if (fromAmount <= 0n || toAmount <= 0n) continue;

      swaps.push({
        pool,
        tokenIn: (fromToken as string).toLowerCase(),
        tokenOut: (toToken as string).toLowerCase(),
        amountIn: fromAmount,
        amountOut: toAmount,
        poolType: POOL_TYPE_DODO,
        blockNumber: Number(log.blockNumber),
        providerName: "dodo",
      });
    }

    return swaps;
  },
};
