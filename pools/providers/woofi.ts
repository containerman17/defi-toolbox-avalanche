import { type Log, decodeAbiParameters, keccak256, toHex } from "viem";
import {
  type PoolProvider,
  type SwapEvent,
  type CachedRPC,
  POOL_TYPE_WOOFI,
} from "../types.ts";

// --- WooFi Router V2 ---
const WOOFI_ROUTER = "0x4c4af8dbc524681930a27b2f1af5bcc8062e6fb7";

// WooRouterSwap(uint8 swapType, address indexed fromToken, address indexed toToken, uint256 fromAmount, uint256 toAmount, address from, address indexed to, address rebateTo)
const WOO_ROUTER_SWAP_TOPIC = keccak256(
  toHex(
    "WooRouterSwap(uint8,address,address,uint256,uint256,address,address,address)",
  ),
);

export const woofiV2: PoolProvider = {
  name: "woofi_v2",
  poolType: POOL_TYPE_WOOFI,
  topics: [WOO_ROUTER_SWAP_TOPIC],

  async processLogs(logs: Log[], _cachedRPC: CachedRPC): Promise<SwapEvent[]> {
    const swaps: SwapEvent[] = [];

    for (const log of logs) {
      if (log.topics[0] !== WOO_ROUTER_SWAP_TOPIC) continue;
      if (log.address.toLowerCase() !== WOOFI_ROUTER) continue;

      const fromToken = ("0x" + log.topics[1]!.slice(26)).toLowerCase();
      const toToken = ("0x" + log.topics[2]!.slice(26)).toLowerCase();

      const [, fromAmount, toAmount] = decodeAbiParameters(
        [
          { type: "uint8", name: "swapType" },
          { type: "uint256", name: "fromAmount" },
          { type: "uint256", name: "toAmount" },
          { type: "address", name: "from" },
          { type: "address", name: "rebateTo" },
        ],
        log.data,
      );

      if (fromAmount <= 0n || toAmount <= 0n) continue;

      swaps.push({
        pool: WOOFI_ROUTER,
        tokenIn: fromToken,
        tokenOut: toToken,
        amountIn: fromAmount,
        amountOut: toAmount,
        poolType: POOL_TYPE_WOOFI,
        blockNumber: Number(log.blockNumber),
        providerName: "woofi_v2",
      });
    }

    return swaps;
  },
};

// --- WooPP Direct (bypasses router) ---
const WOOPP_ADDRESS = "0x5520385bfcf07ec87c4c53a7d8d65595dff69fa4";

// WooSwap(address indexed fromToken, address indexed toToken, uint256 fromAmount, uint256 toAmount, address from, address indexed to, address rebateTo)
const WOOPP_SWAP_TOPIC = keccak256(
  toHex(
    "WooSwap(address,address,uint256,uint256,address,address,address)",
  ),
);

export const woofiPP: PoolProvider = {
  name: "woofi_pp",
  poolType: POOL_TYPE_WOOFI,
  topics: [WOOPP_SWAP_TOPIC],

  async processLogs(logs: Log[], _cachedRPC: CachedRPC): Promise<SwapEvent[]> {
    const swaps: SwapEvent[] = [];

    for (const log of logs) {
      if (log.topics[0] !== WOOPP_SWAP_TOPIC) continue;
      if (log.address.toLowerCase() !== WOOPP_ADDRESS) continue;

      const fromToken = ("0x" + log.topics[1]!.slice(26)).toLowerCase();
      const toToken = ("0x" + log.topics[2]!.slice(26)).toLowerCase();

      const [fromAmount, toAmount] = decodeAbiParameters(
        [
          { type: "uint256", name: "fromAmount" },
          { type: "uint256", name: "toAmount" },
          { type: "address", name: "from" },
          { type: "address", name: "rebateTo" },
        ],
        log.data,
      );

      if (fromAmount <= 0n || toAmount <= 0n) continue;

      swaps.push({
        pool: WOOPP_ADDRESS,
        tokenIn: fromToken,
        tokenOut: toToken,
        amountIn: fromAmount,
        amountOut: toAmount,
        poolType: POOL_TYPE_WOOFI,
        blockNumber: Number(log.blockNumber),
        providerName: "woofi_pp",
      });
    }

    return swaps;
  },
};
