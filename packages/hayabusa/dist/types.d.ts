/** Pool type constants matching the HayabusaRouter contract */
export declare const POOL_TYPE: {
    readonly UNISWAP_V3: 0;
    readonly ALGEBRA: 1;
    readonly LFJ_V1: 2;
    readonly LFJ_V2: 3;
    readonly DODO: 4;
    readonly WOOFI: 5;
    readonly BALANCER_V3: 6;
    readonly PHARAOH_V1: 7;
    readonly V2: 8;
    readonly UNISWAP_V4: 9;
};
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
    stats: {
        totalQuotes: number;
    };
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
//# sourceMappingURL=types.d.ts.map