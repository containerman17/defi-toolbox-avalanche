import { type Log, decodeAbiParameters } from "viem";
import {
  type PoolProvider,
  type SwapEvent,
  type CachedRPC,
  POOL_TYPE_BALANCER_V2,
} from "../types.ts";

const BALANCER_V2_VAULT = "0xba12222222228d8ba445958a75a0704d566bf2c8";

// Swap(bytes32 indexed poolId, address indexed tokenIn, address indexed tokenOut, uint256 amountIn, uint256 amountOut)
const SWAP_TOPIC =
  "0x2170c741c41531aec20e7c107c24eecfdd15e69c9bb0a8dd37b1840b9e0b207b";

export const balancerV2: PoolProvider = {
  name: "balancer_v2",
  poolType: POOL_TYPE_BALANCER_V2,
  topics: [SWAP_TOPIC],

  async processLogs(logs: Log[], _cachedRPC: CachedRPC): Promise<SwapEvent[]> {
    const swaps: SwapEvent[] = [];

    for (const log of logs) {
      if (log.address.toLowerCase() !== BALANCER_V2_VAULT) continue;
      if (log.topics[0] !== SWAP_TOPIC) continue;
      if (!log.topics[1] || !log.topics[2] || !log.topics[3]) continue;

      const poolId = log.topics[1]; // bytes32 poolId
      // Pool address is first 20 bytes of poolId
      const pool = ("0x" + poolId.slice(2, 42)).toLowerCase();
      const tokenIn = ("0x" + log.topics[2].slice(26)).toLowerCase();
      const tokenOut = ("0x" + log.topics[3].slice(26)).toLowerCase();

      const [amountIn, amountOut] = decodeAbiParameters(
        [
          { type: "uint256", name: "amountIn" },
          { type: "uint256", name: "amountOut" },
        ],
        log.data,
      );

      swaps.push({
        pool,
        tokenIn,
        tokenOut,
        amountIn,
        amountOut,
        poolType: POOL_TYPE_BALANCER_V2,
        blockNumber: Number(log.blockNumber),
        providerName: "balancer_v2",
        extraData: `poolId=${poolId}`,
      });
    }

    return swaps;
  },
};
