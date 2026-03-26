export { createQuoter, type Quoter, type QuoterOptions, type FindRouteResult } from "./quoter.js";
export { loadPools, parsePools } from "./pools.js";
export { buildStateOverrides, getBalanceOverride } from "./overrides.js";
export { POOL_TYPE, type StoredPool, type RouteStep, type RouteResult, type PoolType } from "./types.js";
export declare const ROUTER_ADDRESS: string;
export declare const DEPLOYMENT_BLOCK: number;
/** Common token addresses on Avalanche C-Chain */
export declare const TOKENS: {
    readonly WAVAX: "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
    readonly USDC: "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
    readonly USDT: "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7";
    readonly "WETH.e": "0x49d5c2bdffac6ce2bfdb6640f4f80f226bc10bab";
    readonly "USDT.e": "0xc7198437980c041c805a1edcba50c1ce5db95118";
    readonly "USDC.e": "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664";
    readonly "WBTC.e": "0x50b7545627a5162f82a992c33b87adc75187b218";
    readonly "DAI.e": "0xd586e7f844cea2f87f50152665bcbc2c279d8d70";
};
//# sourceMappingURL=index.d.ts.map