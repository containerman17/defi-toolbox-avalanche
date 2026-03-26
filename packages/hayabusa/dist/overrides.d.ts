import { type Hex } from "viem";
/** Compute the storage slot for an ERC20 balance override */
export declare function getBalanceOverride(token: string, amount: bigint, holder?: string): {
    slot: Hex;
    value: Hex;
} | null;
/** Build state overrides for the router to have token balances */
export declare function buildStateOverrides(opts: {
    routerAddress: string;
    tokenAmounts: Map<string, bigint>;
}): Record<string, any>;
//# sourceMappingURL=overrides.d.ts.map