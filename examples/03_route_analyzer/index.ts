// Analyze aggregator swap transactions on Avalanche C-Chain.
// Supports Odos and LFJ routers. Detects split vs single routes via Transfer fan-out.
// Usage: node index.ts [--blocks N] [--router odos|lfj|all] [--tx 0x...]
//   --blocks N          scan last N blocks (default 2000)
//   --router odos|lfj|all  which router to scan (default all)
//   --tx 0x...          analyze specific tx hashes (auto-detects router)

import { createPublicClient, http, type Hex, type Log, formatUnits } from "viem";
import { avalanche } from "viem/chains";

// --- Router definitions ---

interface RouterDef {
  name: string;
  address: string;
  // Topic of the swap event emitted by the router
  swapTopic: string;
  // Extract input/output from the swap event log data
  parseSwapEvent(data: string): { inputToken: string; outputToken: string; inputAmount: bigint; outputAmount: bigint };
}

const ROUTERS: RouterDef[] = [
  {
    name: "odos",
    address: "0x0d05a7d3448512b78fa8a9e46c4872c88c4a0d05",
    // Swap(address sender, uint256 inputAmount, address inputToken, uint256 amountOut, address outputToken, ...)
    swapTopic: "0x69db20ca9e32403e6c56e5193b3e3b2827ae5c430ccfdea392ba950d2d1ab2bc",
    parseSwapEvent(data: string) {
      return {
        inputAmount: BigInt("0x" + data.slice(66, 130)),
        inputToken: ("0x" + data.slice(154, 194)).toLowerCase(),
        outputAmount: BigInt("0x" + data.slice(194, 258)),
        outputToken: ("0x" + data.slice(282, 322)).toLowerCase(),
      };
    },
  },
  {
    name: "lfj",
    address: "0x45a62b090df48243f12a21897e7ed91863e2c86b",
    // LFJ swap event (topic1 = sender)
    // data: sender(32) | inputToken(32) | outputToken(32) | amountIn(32) | amountOut(32)
    swapTopic: "0xd9a8cfa901e597f6bbb7ea94478cf9ad6f38d0dc3fd24d493e99cb40692e39f1",
    parseSwapEvent(data: string) {
      return {
        inputToken: ("0x" + data.slice(90, 130)).toLowerCase(),
        outputToken: ("0x" + data.slice(154, 194)).toLowerCase(),
        inputAmount: BigInt("0x" + data.slice(194, 258)),
        outputAmount: BigInt("0x" + data.slice(258, 322)),
      };
    },
  },
];

const ROUTER_BY_ADDR = new Map(ROUTERS.map(r => [r.address.toLowerCase(), r]));
const ROUTER_BY_NAME = new Map(ROUTERS.map(r => [r.name, r]));

// ERC-20 Transfer(address,address,uint256)
const TRANSFER_TOPIC = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef";
// Uniswap V3/V4 Swap event
const V3_SWAP_TOPIC = "0xc42079f94a6350d7e6235f29174924f928cc2ac818eb64fed8004e115fbcca67";

const KNOWN_TOKENS: Record<string, { symbol: string; decimals: number }> = {
  "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7": { symbol: "WAVAX", decimals: 18 },
  "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e": { symbol: "USDC", decimals: 6 },
  "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7": { symbol: "USDt", decimals: 6 },
  "0x49d5c2bdffac6ce2bfdb6640f4f80f226bc10bab": { symbol: "WETH.e", decimals: 18 },
  "0x2b2c81e08f1af8835a78bb2a90ae924ace0ea4be": { symbol: "sAVAX", decimals: 18 },
  "0x152b9d0fdc40c096de345726706b6d536f291447": { symbol: "ggAVAX", decimals: 18 },
  "0x5c49b268c9841aff1cc3b0a418ff5c3442ee3f3b": { symbol: "MAI", decimals: 18 },
  "0xd586e7f844cea2f87f50152665bcbc2c279d8d70": { symbol: "DAI.e", decimals: 18 },
  "0x50b7545627a5162f82a992c33b87adc75187b218": { symbol: "WBTC.e", decimals: 8 },
  "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664": { symbol: "USDC.e", decimals: 6 },
  "0xc7198437980c041c805a1edcba50c1ce5db95118": { symbol: "USDt.e", decimals: 6 },
};

function tok(addr: string): string {
  const lower = addr.toLowerCase();
  return KNOWN_TOKENS[lower]?.symbol ?? `${lower.slice(0, 6)}..${lower.slice(-4)}`;
}

interface TransferEvent {
  token: string;
  from: string;
  to: string;
  amount: bigint;
}

interface PoolHop {
  pool: string;
  tokenIn: string;
  tokenOut: string;
}

interface TxAnalysis {
  hash: string;
  router: string;
  isSplit: boolean;
  splitReasons: string[];
  inputToken: string;
  outputToken: string;
  inputAmount: bigint;
  outputAmount: bigint;
  pools: PoolHop[];
  gasUsed: bigint;
}

function parseTransfers(logs: Log[]): TransferEvent[] {
  const transfers: TransferEvent[] = [];
  for (const log of logs) {
    if (log.topics[0] === TRANSFER_TOPIC && log.topics.length >= 3) {
      transfers.push({
        token: log.address.toLowerCase(),
        from: ("0x" + log.topics[1]!.slice(26)).toLowerCase(),
        to: ("0x" + log.topics[2]!.slice(26)).toLowerCase(),
        amount: BigInt(log.data),
      });
    }
  }
  return transfers;
}

function detectSplit(transfers: TransferEvent[], excludeAddrs: Set<string>): boolean {
  // A real pool/swap contract both receives and sends tokens.
  // Fee collectors and sinks only receive. Exclude them.
  const senders = new Set<string>();
  for (const t of transfers) senders.add(t.from);

  const fanOut = new Map<string, Set<string>>();
  const ZERO = "0x0000000000000000000000000000000000000000";
  for (const t of transfers) {
    if (excludeAddrs.has(t.from) || excludeAddrs.has(t.to)) continue;
    if (t.from === ZERO) continue;
    // Only count recipients that also send tokens (i.e. pools, not fee sinks)
    if (!senders.has(t.to)) continue;
    const key = `${t.from}:${t.token}`;
    if (!fanOut.has(key)) fanOut.set(key, new Set());
    fanOut.get(key)!.add(t.to);
  }
  for (const [, recipients] of fanOut) {
    if (recipients.size >= 2) return true;
  }
  return false;
}

function debugSplit(transfers: TransferEvent[], excludeAddrs: Set<string>): string[] {
  const senders = new Set<string>();
  for (const t of transfers) senders.add(t.from);

  const fanOut = new Map<string, Set<string>>();
  const ZERO = "0x0000000000000000000000000000000000000000";
  for (const t of transfers) {
    if (excludeAddrs.has(t.from) || excludeAddrs.has(t.to)) continue;
    if (t.from === ZERO) continue;
    if (!senders.has(t.to)) continue;
    const key = `${t.from}:${t.token}`;
    if (!fanOut.has(key)) fanOut.set(key, new Set());
    fanOut.get(key)!.add(t.to);
  }
  const reasons: string[] = [];
  for (const [key, recipients] of fanOut) {
    if (recipients.size >= 2) {
      const [sender, token] = key.split(":");
      reasons.push(`  ${tok(sender!)} sends ${tok(token!)} to ${recipients.size} addrs: ${[...recipients].map(r => r.slice(0, 10)).join(", ")}`);
    }
  }
  return reasons;
}

// Known swap event signatures for pool extraction
const SWAP_EVENT_TOPICS = new Set([
  V3_SWAP_TOPIC,
  "0xd78ad95fa46c994b6551d0da85fc275fe613ce37657fb8d5e3d130840159d822", // V2 Swap
  "0x0874b2d545cb271cdbda4e093020c452328b24af12382ed62c4d00f5c26709db", // Platypus
  "0xdcbc1c05240f31ff3ad067ef1ee35ce4997762752e3a095284754544f4c709d7", // Platypus deposit
  "0x3771d13c67011e31e12031c54bb59b0bf544a80b81d280a3711e172aa8b7f47b", // Platypus/Balancer
  "0x54787c404bb33c88e86f4baf88183a3b0141d0a848e6a9f7a13b66ae3a9b73d1", // LFJ V2.1 Swap
  "0x5303f139d7aacabb0b5c8741d56c117c63c6ee5ba97a9d1c50cb09c423c26c2f", // LFJ V2.2 Swap
  "0x20efd6d5195b7b50273f01cd79a27989255356f9f13293edc53ee142accfdb75", // LFJ V1 Swap (Magpie)
  "0x04da412052b8d39d78da489e294630fcb3874f03dcb0ead4481c0a6d70df1e15", // Platypus Asset swap
]);

function extractPools(logs: Log[]): PoolHop[] {
  const pools: PoolHop[] = [];
  const seen = new Set<string>();
  for (const log of logs) {
    if (log.topics[0] && SWAP_EVENT_TOPICS.has(log.topics[0])) {
      const addr = log.address.toLowerCase();
      if (!seen.has(addr)) {
        seen.add(addr);
        pools.push({ pool: addr, tokenIn: "?", tokenOut: "?" });
      }
    }
  }
  return pools;
}

function resolvePoolTokens(pools: PoolHop[], transfers: TransferEvent[]): void {
  const ZERO = "0x0000000000000000000000000000000000000000";
  for (const hop of pools) {
    const incoming = transfers.filter(t => t.to === hop.pool && t.from !== ZERO);
    const outgoing = transfers.filter(t => t.from === hop.pool && t.to !== ZERO);
    if (incoming.length > 0) hop.tokenIn = incoming[0].token;
    if (outgoing.length > 0) hop.tokenOut = outgoing[0].token;
  }
}

async function analyzeTx(client: ReturnType<typeof createPublicClient>, txHash: Hex, routerHint?: RouterDef): Promise<TxAnalysis | null> {
  const [receipt, tx] = await Promise.all([
    client.getTransactionReceipt({ hash: txHash }),
    client.getTransaction({ hash: txHash }),
  ]);
  if (!receipt || receipt.status !== "success") return null;

  // Auto-detect router from the swap event in logs
  let router = routerHint;
  let swapLog: Log | undefined;

  for (const r of ROUTERS) {
    swapLog = (receipt.logs as Log[]).find(l => l.topics[0] === r.swapTopic && l.address.toLowerCase() === r.address.toLowerCase());
    if (swapLog) { router = r; break; }
  }

  if (!router || !swapLog) return null;

  const { inputToken, outputToken, inputAmount, outputAmount } = router.parseSwapEvent(swapLog.data);
  const transfers = parseTransfers(receipt.logs as Log[]);

  // Exclude router, the user (tx.from), and all known router addresses from fan-out detection.
  // We only care about fan-out at intermediate contract (executor) level.
  const excludeAddrs = new Set([
    router.address.toLowerCase(),
    tx.from.toLowerCase(),
    ...ROUTERS.map(r => r.address.toLowerCase()),
  ]);

  const isSplit = detectSplit(transfers, excludeAddrs);
  const splitReasons = isSplit ? debugSplit(transfers, excludeAddrs) : [];
  const pools = extractPools(receipt.logs as Log[]);
  resolvePoolTokens(pools, transfers);

  return {
    hash: txHash,
    router: router.name,
    isSplit,
    splitReasons,
    inputToken,
    outputToken,
    inputAmount,
    outputAmount,
    pools,
    gasUsed: receipt.gasUsed,
  };
}

function fmtAmount(amount: bigint, token: string): string {
  const info = KNOWN_TOKENS[token.toLowerCase()];
  if (!info) return amount.toString();
  return formatUnits(amount, info.decimals);
}

function printResult(r: TxAnalysis) {
  const splitTag = r.isSplit ? "SPLIT" : "SINGLE";
  const inStr = `${fmtAmount(r.inputAmount, r.inputToken)} ${tok(r.inputToken)}`;
  const outStr = `${fmtAmount(r.outputAmount, r.outputToken)} ${tok(r.outputToken)}`;

  console.log(`${r.hash.slice(0, 10)}  [${r.router}]  [${splitTag}]  ${inStr} → ${outStr}  gas=${r.gasUsed}`);

  if (!r.isSplit && r.pools.length > 0) {
    const route = r.pools.map(p => {
      const tIn = tok(p.tokenIn);
      const tOut = tok(p.tokenOut);
      return `  ${p.pool.slice(0, 10)} ${tIn}→${tOut}`;
    }).join("\n");
    console.log(route);
  }
  if (r.isSplit && r.splitReasons.length > 0) {
    for (const reason of r.splitReasons) console.log(reason);
  }
  console.log();
}

async function main() {
  const args = process.argv.slice(2);
  const rpcUrl = process.env.RPC_URL ?? "https://api.avax.network/ext/bc/C/rpc";

  const client = createPublicClient({
    chain: avalanche,
    transport: http(rpcUrl),
  });

  // Mode 1: analyze specific tx(es)
  const txArgs = args.filter(a => a.startsWith("0x")) as Hex[];
  if (txArgs.length > 0) {
    for (const txHash of txArgs) {
      const result = await analyzeTx(client, txHash);
      if (!result) {
        console.log(`${txHash}: no recognized swap event (reverted, swapMulti, or unknown router)`);
        continue;
      }
      printResult(result);
    }
    return;
  }

  // Mode 2: scan recent blocks
  const blocksBack = parseInt(args.find(a => a.startsWith("--blocks="))?.split("=")[1] ?? "2000");
  const routerFilter = args.find(a => a.startsWith("--router="))?.split("=")[1] ?? "all";

  const routersToScan = routerFilter === "all" ? ROUTERS
    : ROUTER_BY_NAME.has(routerFilter) ? [ROUTER_BY_NAME.get(routerFilter)!]
    : (console.error(`Unknown router: ${routerFilter}. Use: ${ROUTERS.map(r => r.name).join(", ")}, all`), process.exit(1));

  const latestBlock = await client.getBlock({ blockTag: "latest" });
  const latest = latestBlock.number;
  const fromBlock = latest - BigInt(blocksBack);
  if (fromBlock <= 0n) { console.error("Could not get latest block"); return; }

  console.log(`Scanning blocks ${fromBlock}–${latest} for ${routerFilter} swaps...\n`);

  // Collect swap event logs from all selected routers
  const CHUNK = 2048n;
  const allLogs: { log: Log; router: RouterDef }[] = [];

  for (const router of routersToScan) {
    for (let from = fromBlock; from <= latest; from += CHUNK) {
      const to = from + CHUNK - 1n > latest ? latest : from + CHUNK - 1n;
      const chunk = await client.getLogs({
        address: router.address as Hex,
        topics: [router.swapTopic as Hex],
        fromBlock: from,
        toBlock: to,
      });
      for (const log of chunk) allLogs.push({ log: log as Log, router });
    }
  }

  // Sort by block number for chronological output
  allLogs.sort((a, b) => Number((a.log.blockNumber ?? 0n) - (b.log.blockNumber ?? 0n)));

  console.log(`Found ${allLogs.length} swap events\n`);

  let splits = 0;
  let singles = 0;
  const seen = new Set<string>();

  for (const { log, router } of allLogs) {
    const hash = log.transactionHash!;
    if (seen.has(hash)) continue;
    seen.add(hash);

    const result = await analyzeTx(client, hash, router);
    if (!result) continue;
    if (result.isSplit) splits++;
    else singles++;
    printResult(result);
  }

  console.log(`\n--- Summary ---`);
  console.log(`Single routes: ${singles}`);
  console.log(`Split routes:  ${splits}`);
  console.log(`Total:         ${singles + splits}`);
}

main().catch(console.error);
