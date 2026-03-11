export { discover, defaultPoolsPath } from "./discovery.ts";
export { loadPools, parsePools, savePools, serializePools, mergePools } from "./pools.ts";

import { POOL_TYPE_ERC4626 as _ERC4626, POOL_TYPE_BALANCER_V3 as _BAL_V3, POOL_TYPE_BALANCER_V3_BUFFERED as _BAL_V3_BUF, type StoredPool as _StoredPool } from "./types.ts";

export {
  type StoredPool,
  type PoolType,
  type PoolProvider,
  type SwapEvent,
  type CachedRPC,
  POOL_TYPES,
  POOL_TYPE_UNIV3,
  POOL_TYPE_ALGEBRA,
  POOL_TYPE_LFJ_V1,
  POOL_TYPE_LFJ_V2,
  POOL_TYPE_DODO,
  POOL_TYPE_WOOFI,
  POOL_TYPE_BALANCER_V3,
  POOL_TYPE_PHARAOH_V1,
  POOL_TYPE_V2,
  POOL_TYPE_UNIV4,
  POOL_TYPE_ERC4626,
  POOL_TYPE_BALANCER_V3_BUFFERED,
  POOL_TYPE_WOMBAT,
  POOL_TYPE_PLATYPUS,
  POOL_TYPE_WOOPP_V2,
  POOL_TYPE_TRANSFER_FROM,
  POOL_TYPE_BALANCER_V2,
  POOL_TYPE_CAVALRE,
  POOL_TYPE_KYBER_DMM,
  POOL_TYPE_SYNAPSE,
  POOL_TYPE_TRIDENT,
} from "./types.ts";

/**
 * Known ERC-4626 wrapper vaults on Avalanche (Aave V3 wrapped aTokens).
 * These are modeled as virtual pools: deposit(underlying→shares) / redeem(shares→underlying).
 */
export const ERC4626_VAULTS: _StoredPool[] = [
  {
    address: "0xd7da0de6ef4f51d6206bf2a35fcd2030f54c3f7b", // waAvaWAVAX
    providerName: "erc4626",
    poolType: _ERC4626,
    tokens: [
      "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7", // WAVAX (underlying)
      "0xd7da0de6ef4f51d6206bf2a35fcd2030f54c3f7b", // waAvaWAVAX (shares)
    ],
    latestSwapBlock: 999999999, // Always high priority
  },
  {
    address: "0x7d0394f8898fba73836bf12bd606228887705895", // waAvaSAVAX
    providerName: "erc4626",
    poolType: _ERC4626,
    tokens: [
      "0x2b2c81e08f1af8835a78bb2a90ae924ace0ea4be", // sAVAX (underlying)
      "0x7d0394f8898fba73836bf12bd606228887705895", // waAvaSAVAX (shares)
    ],
    latestSwapBlock: 999999999,
  },
  {
    address: "0xe1bfc96d95badcb10ff013cb0c9c6c737ca07009", // waAvaUSDC
    providerName: "erc4626",
    poolType: _ERC4626,
    tokens: [
      "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e", // USDC (underlying)
      "0xe1bfc96d95badcb10ff013cb0c9c6c737ca07009", // waAvaUSDC (shares)
    ],
    latestSwapBlock: 999999999,
  },
  {
    address: "0x59933c571d200dc6a7fd1cda22495db442082e34", // waAvaUSDT
    providerName: "erc4626",
    poolType: _ERC4626,
    tokens: [
      "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7", // USDt (underlying)
      "0x59933c571d200dc6a7fd1cda22495db442082e34", // waAvaUSDT (shares)
    ],
    latestSwapBlock: 999999999,
  },
  {
    address: "0x45cf39eeb437fa95bb9b52c0105254a6bd25d01e", // waAvaAUSD (Wrapped Aave Avalanche AUSD)
    providerName: "erc4626",
    poolType: _ERC4626,
    tokens: [
      "0x00000000efe302beaa2b3e6e1b18d08d69a9012a", // AUSD (underlying)
      "0x45cf39eeb437fa95bb9b52c0105254a6bd25d01e", // waAvaAUSD (shares)
    ],
    latestSwapBlock: 999999999,
  },
  {
    address: "0x2d324fd1ca86d90f61b0965d2db2f86d22ea4b74", // waAvaBTC.b
    providerName: "erc4626",
    poolType: _ERC4626,
    tokens: [
      "0x152b9d0fFdD62421C7C990829b2B257108600162", // BTC.b (underlying)
      "0x2d324fd1ca86d90f61b0965d2db2f86d22ea4b74", // waAvaBTC.b (shares)
    ],
    latestSwapBlock: 999999999,
  },
  {
    address: "0xa25eaf2906fa1a3a13edac9b9657108af7b703e3", // ggAVAX / Hypha Staked AVAX
    providerName: "erc4626",
    poolType: _ERC4626,
    tokens: [
      "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7", // WAVAX (underlying)
      "0xa25eaf2906fa1a3a13edac9b9657108af7b703e3", // ggAVAX (shares)
    ],
    latestSwapBlock: 999999999,
  },
];

/**
 * Build a mapping from wrapped token address → underlying token address
 * from the ERC4626_VAULTS list.
 */
const wrappedToUnderlying = new Map<string, string>();
for (const v of ERC4626_VAULTS) {
  // tokens[0] = underlying, tokens[1] = wrapped (same as vault address)
  wrappedToUnderlying.set(v.address.toLowerCase(), v.tokens[0].toLowerCase());
}

/**
 * Generate synthetic Balancer V3 Buffered edges (type 11) for pools whose tokens
 * are ERC4626 wrappers. For each pair of wrapped tokens in a Balancer V3 pool,
 * emit an edge between their underlying tokens.
 */
export function generateBufferedEdges(pools: Iterable<_StoredPool>): _StoredPool[] {
  const edges: _StoredPool[] = [];
  for (const pool of pools) {
    if (pool.poolType !== _BAL_V3) continue;
    // Find which tokens in this pool are ERC4626 wrappers
    const wrapped = pool.tokens
      .map(t => t.toLowerCase())
      .filter(t => wrappedToUnderlying.has(t));
    if (wrapped.length < 2) continue;
    // Generate edges for each ordered pair of wrapped tokens
    for (const wi of wrapped) {
      for (const wo of wrapped) {
        if (wi === wo) continue;
        const underlyingIn = wrappedToUnderlying.get(wi)!;
        const underlyingOut = wrappedToUnderlying.get(wo)!;
        edges.push({
          address: pool.address.toLowerCase(),
          providerName: "balancer_v3_buffered",
          poolType: _BAL_V3_BUF,
          tokens: [underlyingIn, underlyingOut],
          latestSwapBlock: 999999999,
          extraData: `wi=${wi},bp=${pool.address.toLowerCase()},wo=${wo}`,
        });
      }
    }
  }
  return edges;
}
