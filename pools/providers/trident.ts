import { type Log, keccak256, toHex } from "viem";
import {
  type PoolProvider,
  type SwapEvent,
  type CachedRPC,
  POOL_TYPE_TRIDENT,
} from "../types.ts";

// BentoBoxV1 vault on Avalanche — Trident pools live inside BentoBox.
const BENTOBOX = "0x0711b6026068f736bae6b213031fce978d48e026";

// Trident ConstantProductPool emits: Swap(address recipient, address tokenIn, address tokenOut, uint256 amountIn, uint256 amountOut)
// All 3 address params are indexed (in topics[1..3]), amountIn/amountOut are in data.
const TRIDENT_SWAP_TOPIC = keccak256(
  toHex("Swap(address,address,address,uint256,uint256)"),
);

// Cache of verified Trident pools (confirmed to have bento() returning BentoBox)
const verifiedPools = new Set<string>();
const notTridentPools = new Set<string>();

export const trident: PoolProvider = {
  name: "trident",
  poolType: POOL_TYPE_TRIDENT,
  topics: [TRIDENT_SWAP_TOPIC],

  async processLogs(logs: Log[], cachedRPC: CachedRPC): Promise<SwapEvent[]> {
    const swaps: SwapEvent[] = [];

    for (const log of logs) {
      if (log.topics[0] !== TRIDENT_SWAP_TOPIC) continue;
      // Trident Swap has exactly 4 topics: event sig + 3 indexed addresses
      if (log.topics.length !== 4) continue;

      const pool = log.address.toLowerCase();

      // Skip pools we already know are not Trident
      if (notTridentPools.has(pool)) continue;

      // Verify this is actually a Trident pool by checking bento()
      if (!verifiedPools.has(pool)) {
        try {
          const bentoAddr = await cachedRPC.getAddress(pool, "bento()");
          if (bentoAddr.toLowerCase() !== BENTOBOX) {
            notTridentPools.add(pool);
            continue;
          }
          verifiedPools.add(pool);
        } catch {
          notTridentPools.add(pool);
          continue;
        }
      }

      const tokenIn = ("0x" + log.topics[2]!.slice(26)).toLowerCase();
      const tokenOut = ("0x" + log.topics[3]!.slice(26)).toLowerCase();

      // data = abi.encode(uint256 amountIn, uint256 amountOut)
      const data = log.data.slice(2); // remove 0x
      if (data.length < 128) continue;
      const amountIn = BigInt("0x" + data.slice(0, 64));
      const amountOut = BigInt("0x" + data.slice(64, 128));

      if (amountIn <= 0n || amountOut <= 0n) continue;

      swaps.push({
        pool,
        tokenIn,
        tokenOut,
        amountIn,
        amountOut,
        poolType: POOL_TYPE_TRIDENT,
        blockNumber: Number(log.blockNumber),
        providerName: "trident",
        // Store BentoBox address in extraData for router encoding
        extraData: `bento=${BENTOBOX}`,
      });
    }

    return swaps;
  },
};
