import type { StoredPool } from "./types.js";
/** Parse pools.txt content into StoredPool array */
export declare function parsePools(content: string): {
    headBlock: number;
    pools: StoredPool[];
};
/** Load pools from the bundled pools.txt or a custom path */
export declare function loadPools(path?: string, limit?: number): StoredPool[];
//# sourceMappingURL=pools.d.ts.map