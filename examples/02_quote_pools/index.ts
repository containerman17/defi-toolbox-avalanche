import {
    loadPools,
    defaultPoolsPath,
    type StoredPool,
} from "../../pools/index.ts";
import { quoteRoute, ROUTER_ADDRESS } from "../../router/index.ts";
import { createPublicClient, http, formatUnits } from "viem";
import { avalanche } from "viem/chains";
import { loadDotEnv } from "../../utils/env.ts";

loadDotEnv();

const WAVAX = "0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7".toLowerCase();
const USDC = "0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E".toLowerCase();
const TENTH_AVAX = 100_000_000_000_000_000n; // 0.1e18

const rpcUrl = process.env.RPC_URL || "https://api.avax.network/ext/bc/C/rpc";
const poolsPath = process.argv[2] || defaultPoolsPath();

const { pools } = loadPools(poolsPath);

// Filter pools that contain both WAVAX and USDC
const wavaxUsdcPools: StoredPool[] = [];
for (const pool of pools.values()) {
    const tokens = pool.tokens.map((t) => t.toLowerCase());
    if (tokens.includes(WAVAX) && tokens.includes(USDC)) {
        wavaxUsdcPools.push(pool);
    }
}

// Sort by most recent swap, take top 10
wavaxUsdcPools.sort((a, b) => b.latestSwapBlock - a.latestSwapBlock);
const top10 = wavaxUsdcPools.slice(0, 30);

console.log(
    `Found ${wavaxUsdcPools.length} WAVAX/USDC pools, quoting top ${top10.length} by recent activity\n`,
);

const client = createPublicClient({
    chain: avalanche,
    transport: http(rpcUrl),
});

const start = performance.now();

const settled = await Promise.allSettled(
    top10.map((pool) => {
        const route = [{ pool, tokenIn: WAVAX, tokenOut: USDC }];
        return quoteRoute(client, route, TENTH_AVAX).then((amountOut) => ({ pool, amountOut }));
    }),
);

const elapsed = (performance.now() - start).toFixed(0);

const results: { pool: StoredPool; amountOut: bigint }[] = [];
for (let i = 0; i < settled.length; i++) {
    const r = settled[i];
    if (r.status === "fulfilled") {
        results.push(r.value);
    } else {
        const pool = top10[i];
        console.log(
            `${pool.providerName.padEnd(16)} ${pool.address}  block ${pool.latestSwapBlock}  →  ERROR: ${r.reason?.message?.slice(0, 80)}`,
        );
    }
}

const best = results.reduce((a, b) => (a.amountOut > b.amountOut ? a : b)).amountOut;

for (const { pool, amountOut } of results) {
    const price = formatUnits(amountOut * 10n, 6);
    const diff = (Number(amountOut - best) / Number(best)) * 100;
    const diffStr = diff === 0 ? "  BEST" : `${diff.toFixed(2)}%`;
    console.log(
        `${pool.providerName.padEnd(16)} ${pool.address}  block ${pool.latestSwapBlock}  →  $${price.padStart(10)}  ${diffStr}`,
    );
}

console.log(`\nQuoted ${top10.length} pools in ${elapsed}ms`);
