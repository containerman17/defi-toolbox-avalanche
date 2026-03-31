export { discover, defaultPoolsPath } from "./discovery.ts";
export { loadPools, parsePools, savePools, serializePools, mergePools } from "./pools.ts";
export { ERC4626_VAULTS, generateBufferedEdges } from "./erc4626.ts";

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

// ── CLI entry point ─────────────────────────────────────────────────

if (import.meta.filename === process.argv[1]) {
    const path = await import("path");
    const fs = await import("fs");
    const { discover, defaultPoolsPath } = await import("./discovery.ts");

    // Load .env from current dir and all parent dirs
    let dir = process.cwd();
    while (true) {
        const envPath = path.join(dir, ".env");
        if (fs.existsSync(envPath)) process.loadEnvFile(envPath);
        const parent = path.dirname(dir);
        if (parent === dir) break;
        dir = parent;
    }

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
}
