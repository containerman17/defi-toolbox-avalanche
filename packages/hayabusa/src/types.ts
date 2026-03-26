/** Pool type constants matching the HayabusaRouter contract */
export const POOL_TYPE = {
  UNISWAP_V3: 0,
  ALGEBRA: 1,
  LFJ_V1: 2,
  LFJ_V2: 3,
  DODO: 4,
  WOOFI: 5,
  BALANCER_V3: 6,
  PHARAOH_V1: 7,
  V2: 8,
  UNISWAP_V4: 9,
} as const;

export type PoolType = (typeof POOL_TYPE)[keyof typeof POOL_TYPE];

/** A pool from pools.txt */
export interface StoredPool {
  address: string;
  providerName: string;
  poolType: PoolType;
  tokens: string[];
  latestSwapBlock: number;
  extraData?: string;
}

/** A single hop in a route */
export interface RouteStep {
  pool: StoredPool;
  tokenIn: string;
  tokenOut: string;
}

/** Result of findRoute */
export interface RouteResult {
  route: RouteStep[];
  amountOut: bigint;
  stats: { totalQuotes: number };
}

/** Native harness route step (raw JSON) */
export interface NativeRouteStep {
  pool: string;
  poolType: number;
  tokenIn: string;
  tokenOut: string;
}

/** Native harness route result (raw JSON) */
export interface NativeRouteResult {
  steps: NativeRouteStep[];
  amountOut: string;
  stats: {
    formulaQuotes: number;
    evmQuotes: number;
    totalQuotes: number;
    formulaMs: number;
    evmMs: number;
  };
}
