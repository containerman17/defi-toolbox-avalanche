import { readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
const __dirname = dirname(fileURLToPath(import.meta.url));
const DATA_DIR = join(__dirname, "..", "data");
/** Map from provider name to pool type ID */
const POOL_TYPE_MAP = {
    uniswap_v3: 0, pharaoh_v3: 0,
    algebra: 1,
    lfj_v1: 2,
    lfj_v2: 3,
    dodo: 4,
    woofi: 5, woofi_v2: 5,
    balancer_v3: 6,
    pharaoh_v1: 7,
    pangolin_v2: 8, arena_v2: 8, uniswap_v2: 8, sushiswap_v2: 8,
    elkdex: 8, lydia: 8, vapordex: 8, swapsicle: 8,
    canary: 8, yetiswap: 8, oliveswap: 8, partyswap: 8,
    complus: 8, hurricane: 8, radioshack: 8, thorus: 8,
    hakuswap: 8, zeroex: 8, fraxswap: 8,
    uniswap_v4: 9,
};
/** Parse pools.txt content into StoredPool array */
export function parsePools(content) {
    const lines = content.split("\n");
    let headBlock = 0;
    const pools = [];
    for (let i = 0; i < lines.length; i++) {
        const line = lines[i].trim();
        if (!line)
            continue;
        if (i === 0) {
            headBlock = parseInt(line, 10);
            continue;
        }
        const parts = line.split(":");
        if (parts.length < 5)
            continue;
        const address = parts[0].toLowerCase();
        const providerName = parts[1];
        const poolTypeId = POOL_TYPE_MAP[providerName];
        if (poolTypeId === undefined)
            continue;
        const latestSwapBlock = parseInt(parts[3], 10);
        // Tokens start at index 4, extra data starts with @
        const tokens = [];
        let extraData;
        for (let j = 4; j < parts.length; j++) {
            if (parts[j].startsWith("@")) {
                extraData = parts[j].slice(1);
            }
            else if (parts[j].startsWith("0x") && parts[j].length === 42) {
                tokens.push(parts[j].toLowerCase());
            }
        }
        if (tokens.length < 2)
            continue;
        pools.push({
            address,
            providerName,
            poolType: poolTypeId,
            tokens,
            latestSwapBlock,
            extraData,
        });
    }
    return { headBlock, pools };
}
/** Load pools from the bundled pools.txt or a custom path */
export function loadPools(path, limit) {
    const filePath = path || join(DATA_DIR, "pools.txt");
    const content = readFileSync(filePath, "utf-8");
    const { pools } = parsePools(content);
    return limit ? pools.slice(0, limit) : pools;
}
//# sourceMappingURL=pools.js.map