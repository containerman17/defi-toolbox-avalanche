import { discover, defaultPoolsPath } from "../../pools/index.ts";

import { loadDotEnv } from "../../utils/env.ts";

loadDotEnv();

const archivalRpcUrl =
    process.env.ARCHIVAL_RPC_URL || "https://api.avax.network/ext/bc/C/rpc";
const rpcUrl = process.env.RPC_URL || archivalRpcUrl;
const poolsPath = process.argv[2] || defaultPoolsPath();

console.log("Updating pool list...");
console.log(`RPC:           ${rpcUrl}`);
console.log(`Archival RPC:  ${archivalRpcUrl}`);
console.log(`Pools:         ${poolsPath}`);

let startTime = 0;
let firstBlock = 0;

const result = await discover({
    archivalRpcUrl,
    rpcUrl,
    poolsPath,
    onProgress: ({ fromBlock, toBlock, headBlock, totalPools, newSwaps }) => {
        if (!startTime) {
            startTime = Date.now();
            firstBlock = fromBlock;
        }

        const elapsed = (Date.now() - startTime) / 1000;
        const scanned = toBlock - firstBlock;
        const remaining = headBlock - toBlock;
        const blocksPerSec = scanned / (elapsed || 1);
        const etaSec = Math.round(remaining / (blocksPerSec || 1));
        const etaMin = (etaSec / 60).toFixed(1);

        console.log(
            `${fromBlock}-${toBlock} | ${newSwaps} swaps | ${totalPools} pools | ${remaining} behind | ETA ${etaMin}m`,
        );
    },
});

console.log(
    `\nDone: ${result.totalPools} pools (+${result.newPools} new), ${result.blocksScanned} blocks scanned`,
);
