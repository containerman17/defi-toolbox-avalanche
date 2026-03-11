import { type Log, decodeAbiParameters, keccak256, toHex } from "viem";
import {
  type PoolProvider,
  type SwapEvent,
  type CachedRPC,
  POOL_TYPE_WOMBAT,
  POOL_TYPE_PLATYPUS,
} from "../types.ts";

// Shared Swap event for both Wombat and Platypus:
// Swap(address indexed sender, address fromToken, address toToken, uint256 fromAmount, uint256 toAmount, address indexed to)
const WOMBAT_PLATYPUS_SWAP_TOPIC = keccak256(
  toHex("Swap(address,address,address,uint256,uint256,address)"),
);

// Known Wombat pool addresses on Avalanche
const WOMBAT_POOLS = new Set([
  "0xe3abc29b035874a9f6dcdb06f8f20d9975069d87", // WAVAX/sAVAX
]);

// Known Platypus pool addresses on Avalanche
const PLATYPUS_POOLS = new Set([
  "0x5ee9008e49b922cafef9dde21446934547e42ad6", // Main stableswap
]);

type PoolClassification = "wombat" | "platypus" | null;

/**
 * Identify whether a contract is a Wombat pool, Platypus pool, or neither.
 *
 * Strategy:
 * - First check known address sets (fast path)
 * - Otherwise probe: Wombat pools have getTokens(), Platypus pools have getTokenAddresses()
 *   Both are multi-asset pools with the same Swap event signature.
 */
async function classifyPool(
  pool: string,
  cachedRPC: CachedRPC,
  classified: Map<string, PoolClassification>,
  rejected: Set<string>,
): Promise<PoolClassification> {
  if (rejected.has(pool)) return null;
  const cached = classified.get(pool);
  if (cached !== undefined) return cached;

  // Fast path: check known sets
  if (WOMBAT_POOLS.has(pool)) {
    classified.set(pool, "wombat");
    return "wombat";
  }
  if (PLATYPUS_POOLS.has(pool)) {
    classified.set(pool, "platypus");
    return "platypus";
  }

  // Probe: try getTokens() (Wombat) then getTokenAddresses() (Platypus)
  try {
    const result = await cachedRPC.ethCall(pool, "getTokens()");
    // Valid if it returns a non-empty array (offset + length + at least one address = 96+ bytes)
    if (result && result.length >= 2 + 64 * 3) {
      classified.set(pool, "wombat");
      return "wombat";
    }
  } catch {
    // Not a Wombat pool
  }

  try {
    const result = await cachedRPC.ethCall(pool, "getTokenAddresses()");
    if (result && result.length >= 2 + 64 * 3) {
      classified.set(pool, "platypus");
      return "platypus";
    }
  } catch {
    // Not a Platypus pool
  }

  rejected.add(pool);
  return null;
}

/** Parse an ABI-encoded address[] from raw hex result */
function decodeAddressArray(hex: string): string[] {
  // Remove 0x prefix
  const data = hex.slice(2);
  // First 32 bytes = offset to array data (always 0x20 for a single dynamic param)
  // Next 32 bytes = array length
  const length = parseInt(data.slice(64, 128), 16);
  const addresses: string[] = [];
  for (let i = 0; i < length; i++) {
    const start = 128 + i * 64;
    const addr = "0x" + data.slice(start + 24, start + 64).toLowerCase();
    addresses.push(addr);
  }
  return addresses;
}

/** Fetch all tokens for a Wombat or Platypus pool */
async function getPoolTokens(
  pool: string,
  classification: PoolClassification,
  cachedRPC: CachedRPC,
): Promise<string[]> {
  const method = classification === "wombat" ? "getTokens()" : "getTokenAddresses()";
  const result = await cachedRPC.ethCall(pool, method);
  return decodeAddressArray(result);
}

function createWombatPlatypusProvider(
  name: string,
  targetClassification: "wombat" | "platypus",
): PoolProvider {
  const classified = new Map<string, PoolClassification>();
  const rejected = new Set<string>();
  const poolTokensCache = new Map<string, string[]>();

  const poolType = targetClassification === "wombat" ? POOL_TYPE_WOMBAT : POOL_TYPE_PLATYPUS;

  return {
    name,
    poolType,
    topics: [WOMBAT_PLATYPUS_SWAP_TOPIC],

    async processLogs(logs: Log[], cachedRPC: CachedRPC): Promise<SwapEvent[]> {
      const swaps: SwapEvent[] = [];

      // Group logs by emitting address
      const logsByPool = new Map<string, Log[]>();
      for (const log of logs) {
        if (log.topics[0] !== WOMBAT_PLATYPUS_SWAP_TOPIC) continue;
        const pool = log.address.toLowerCase();
        if (!logsByPool.has(pool)) logsByPool.set(pool, []);
        logsByPool.get(pool)!.push(log);
      }

      for (const [pool, poolLogs] of logsByPool) {
        const cls = await classifyPool(pool, cachedRPC, classified, rejected);
        if (cls !== targetClassification) continue;

        // Ensure we have the pool's token list cached (for validation)
        if (!poolTokensCache.has(pool)) {
          try {
            const tokens = await getPoolTokens(pool, cls, cachedRPC);
            poolTokensCache.set(pool, tokens);
          } catch {
            continue;
          }
        }

        for (const log of poolLogs) {
          try {
            // Data layout: fromToken (address), toToken (address), fromAmount (uint256), toAmount (uint256)
            // sender is indexed (topic1), to is indexed (topic2)
            const [fromToken, toToken, fromAmount, toAmount] = decodeAbiParameters(
              [
                { type: "address", name: "fromToken" },
                { type: "address", name: "toToken" },
                { type: "uint256", name: "fromAmount" },
                { type: "uint256", name: "toAmount" },
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

export const wombat = createWombatPlatypusProvider("wombat", "wombat");
export const platypus = createWombatPlatypusProvider("platypus", "platypus");
