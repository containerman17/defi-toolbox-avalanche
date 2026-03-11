import { type Log } from "viem";
import { type PoolProvider, type SwapEvent, type StoredPool } from "./types.ts";
import { CachedRpcClient } from "./cached-rpc.ts";
import { loadPools, savePools, mergePools } from "./pools.ts";
import { providers, seedV4Pools } from "./providers/index.ts";
import * as fs from "fs";
import path from "path";

const DEFAULT_BATCH_SIZE = 2_000;
const DEFAULT_START_BACK = 1_000_000;

interface DiscoveryOptions {
  /** Archival RPC URL for eth_getLogs (must support large block ranges) */
  archivalRpcUrl: string;
  /** Regular RPC URL for eth_call (factory verification). Defaults to archivalRpcUrl. */
  rpcUrl?: string;
  /** Path to pools.txt file. Defaults to data/pools.txt in the package. */
  poolsPath?: string;
  /** Explicit start block. Overrides the saved head block when set. */
  startBlock?: number;
  /** Blocks per eth_getLogs batch. Default: 10000 */
  batchSize?: number;
  /** Progress callback */
  onProgress?: (info: {
    fromBlock: number;
    toBlock: number;
    headBlock: number;
    totalPools: number;
    newSwaps: number;
  }) => void;
  /** AbortSignal to stop discovery gracefully */
  signal?: AbortSignal;
}

/** Get the default pools.txt path (shipped with the package) */
export function defaultPoolsPath(): string {
  return path.join(import.meta.dirname, "data/pools.txt");
}

/** Get current block number from RPC */
async function getBlockNumber(rpcUrl: string): Promise<number> {
  const resp = await fetch(rpcUrl, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      jsonrpc: "2.0",
      id: 1,
      method: "eth_blockNumber",
      params: [],
    }),
  });
  const json = (await resp.json()) as { result: string };
  return parseInt(json.result, 16);
}

/** Fetch logs from RPC with retry */
async function getLogsWithRetry(
  rpcUrl: string,
  from: number,
  to: number,
  topics: string[],
  maxRetries = 3,
): Promise<Log[]> {
  for (let attempt = 0; attempt < maxRetries; attempt++) {
    try {
      const resp = await fetch(rpcUrl, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          jsonrpc: "2.0",
          id: 1,
          method: "eth_getLogs",
          params: [
            {
              fromBlock: `0x${from.toString(16)}`,
              toBlock: `0x${to.toString(16)}`,
              topics: [topics],
            },
          ],
        }),
      });
      const json = (await resp.json()) as {
        result?: Log[];
        error?: { message: string };
      };
      if (json.error) throw new Error(json.error.message);
      return json.result || [];
    } catch (err) {
      if (attempt === maxRetries - 1) throw err;
      const delay = Math.pow(2, attempt) * 1000;
      console.error(
        `Retry ${attempt + 1}/${maxRetries} for blocks ${from}-${to}: ${err}`,
      );
      await new Promise((r) => setTimeout(r, delay));
    }
  }
  return [];
}

/** Collect all unique topics from all providers */
function allTopics(): string[] {
  const set = new Set<string>();
  for (const p of providers) {
    for (const t of p.topics) {
      set.add(t);
    }
  }
  return [...set];
}

/** Process raw logs through all providers, return swap events */
async function processLogs(
  logs: Log[],
  allProviders: PoolProvider[],
  cachedRPC: CachedRpcClient,
): Promise<SwapEvent[]> {
  const results = await Promise.all(
    allProviders.map((p) => p.processLogs(logs, cachedRPC)),
  );
  return results.flat();
}

/** Convert swap events to StoredPool updates */
function swapEventsToPoolUpdates(
  events: SwapEvent[],
): StoredPool[] {
  const poolMap = new Map<string, StoredPool>();

  for (const evt of events) {
    const key = evt.pool.toLowerCase();
    const existing = poolMap.get(key);
    if (existing) {
      // Add tokens
      for (const t of [evt.tokenIn, evt.tokenOut]) {
        if (!existing.tokens.includes(t)) {
          existing.tokens.push(t);
        }
      }
      existing.latestSwapBlock = Math.max(
        existing.latestSwapBlock,
        evt.blockNumber,
      );
    } else {
      poolMap.set(key, {
        address: key,
        providerName: evt.providerName,
        poolType: evt.poolType,
        tokens: [evt.tokenIn, evt.tokenOut],
        latestSwapBlock: evt.blockNumber,
        extraData: evt.extraData,
      });
    }
  }

  return [...poolMap.values()];
}

/**
 * Run incremental pool discovery.
 * Scans from the last processed block (stored in pools.txt header) to chain head.
 * Saves progress periodically and on completion.
 */
export async function discover(options: DiscoveryOptions): Promise<{
  totalPools: number;
  newPools: number;
  blocksScanned: number;
}> {
  const {
    archivalRpcUrl,
    rpcUrl = archivalRpcUrl,
    poolsPath = defaultPoolsPath(),
    startBlock: explicitStartBlock,
    batchSize = DEFAULT_BATCH_SIZE,
    onProgress,
    signal,
  } = options;

  const cachedRPC = new CachedRpcClient(rpcUrl);
  const topics = allTopics();

  // Load existing pools
  let headBlock: number;
  let pools: Map<string, StoredPool>;
  let startBlock: number;

  if (fs.existsSync(poolsPath)) {
    const loaded = loadPools(poolsPath);
    headBlock = loaded.headBlock;
    pools = loaded.pools;
    startBlock = headBlock + 1;
    console.error(`Loaded ${pools.size} pools, resuming from block ${startBlock}`);
  } else {
    // Copy bundled snapshot from the package, then scan forward
    const bundledPath = defaultPoolsPath();
    if (fs.existsSync(bundledPath)) {
      fs.mkdirSync(path.dirname(poolsPath), { recursive: true });
      fs.copyFileSync(bundledPath, poolsPath);
      const loaded = loadPools(poolsPath);
      headBlock = loaded.headBlock;
      pools = loaded.pools;
      startBlock = headBlock + 1;
      console.error(`Copied bundled snapshot (${pools.size} pools), resuming from block ${startBlock}`);
    } else {
      pools = new Map();
      const chainHead = await getBlockNumber(archivalRpcUrl);
      startBlock = Math.max(1, chainHead - DEFAULT_START_BACK);
      headBlock = 0;
      console.error(`No pools file or bundled snapshot, starting from block ${startBlock}`);
    }
  }

  if (explicitStartBlock !== undefined) {
    startBlock = explicitStartBlock;
    console.error(`Using explicit start block ${startBlock}`);
  }

  const seeded = seedV4Pools(pools.values());
  if (seeded.legacyWithoutId > 0) {
    throw new Error(
      `Found ${seeded.legacyWithoutId} legacy Uniswap V4 pools in ${poolsPath} without persisted pool ids. Full rescan required: delete or replace the pools file and rerun discovery from historical blocks.`,
    );
  }
  console.error(`Seeded ${seeded.seeded} V4 pools from pools.txt`);

  const chainHead = await getBlockNumber(archivalRpcUrl);
  const initialPoolCount = pools.size;
  let totalSwaps = 0;
  let lastSave = Date.now();

  for (
    let currentBlock = startBlock;
    currentBlock <= chainHead;
    currentBlock += batchSize
  ) {
    if (signal?.aborted) break;

    const batchEnd = Math.min(currentBlock + batchSize - 1, chainHead);

    const logs = await getLogsWithRetry(
      archivalRpcUrl,
      currentBlock,
      batchEnd,
      topics,
    );

    const events = await processLogs(logs, providers, cachedRPC);
    totalSwaps += events.length;

    if (events.length > 0) {
      const updates = swapEventsToPoolUpdates(events);
      mergePools(pools, updates);
    }

    onProgress?.({
      fromBlock: currentBlock,
      toBlock: batchEnd,
      headBlock: chainHead,
      totalPools: pools.size,
      newSwaps: events.length,
    });

    // Save every 10 seconds
    if (Date.now() - lastSave > 10_000) {
      savePools(poolsPath, batchEnd, pools.values());
      lastSave = Date.now();
    }
  }

  // Final save
  savePools(poolsPath, chainHead, pools.values());

  return {
    totalPools: pools.size,
    newPools: pools.size - initialPoolCount,
    blocksScanned: chainHead - startBlock + 1,
  };
}

// CLI entry point
if (
  import.meta.url === `file://${process.argv[1]}` ||
  process.argv[1]?.endsWith("discovery.ts")
) {
  const archivalRpcUrl =
    process.env.ARCHIVAL_RPC_URL ||
    "https://api.avax.network/ext/bc/C/rpc";
  const rpcUrl = process.env.RPC_URL || archivalRpcUrl;
  const poolsPath = process.argv[2] || defaultPoolsPath();
  const startBlockArg = process.argv[3] || process.env.DISCOVERY_START_BLOCK;
  const startBlock =
    startBlockArg !== undefined ? Number(startBlockArg) : undefined;

  console.error("Starting pool discovery...");
  console.error(`Archival RPC: ${archivalRpcUrl}`);
  console.error(`Pools: ${poolsPath}`);
  if (startBlock !== undefined && !Number.isNaN(startBlock)) {
    console.error(`Start block override: ${startBlock}`);
  }

  const result = await discover({
    archivalRpcUrl,
    rpcUrl,
    poolsPath,
    startBlock: Number.isNaN(startBlock) ? undefined : startBlock,
    onProgress: ({ fromBlock, toBlock, headBlock, totalPools, newSwaps }) => {
      const behind = headBlock - toBlock;
      console.error(
        `blocks ${fromBlock}-${toBlock} | ${newSwaps} swaps | total=${totalPools} pools | behind=${behind}`,
      );
    },
  });

  console.error(
    `\nDone: ${result.totalPools} pools (+${result.newPools} new), ${result.blocksScanned} blocks scanned`,
  );
}
