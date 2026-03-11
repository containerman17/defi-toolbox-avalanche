// Race our BFS against a captured Odos snapshot.
// Usage: node race.ts <capture_file>
//   e.g. node race.ts captures/80336680.json
//
// Set STATE_CACHE_DIR to enable Rust-side state caching for fast repeat runs.
// e.g. STATE_CACHE_DIR=./state_cache node race.ts captures/80336680.json

import { spawn } from "node:child_process";
import * as fs from "node:fs";
import path from "node:path";
import { createPublicClient, http, formatUnits, type Hex, decodeAbiParameters } from "viem";
import { avalanche } from "viem/chains";
import { loadDotEnv } from "../../utils/env.ts";
import {
  loadPools, defaultPoolsPath, serializePools, type StoredPool, ERC4626_VAULTS, generateBufferedEdges,
} from "../../pools/index.ts";
import { ROUTER_ADDRESS, quoteRoute } from "../../router/index.ts";
import { encodeSwap, type RouteStep } from "../../router/encode.ts";
import { getBalanceOverride, getAllowanceOverride } from "../../router/overrides.ts";

loadDotEnv();

// ── Args ──
const captureFile = process.argv[2];
if (!captureFile) {
  console.error("Usage: node race.ts <capture_file>");
  console.error("  e.g. node race.ts captures/80336680.json");
  process.exit(1);
}

const capture = JSON.parse(fs.readFileSync(captureFile, "utf-8"));
const block = BigInt(capture.block);
console.log(`Racing against Odos capture at block ${block}`);
console.log(`Captured at: ${capture.timestamp}\n`);

// ── Load pools ──
const POOL_LIMIT = parseInt(process.env.POOL_LIMIT || "7500");
const { pools, headBlock } = loadPools(defaultPoolsPath());
const allPools = [...pools.values()];
allPools.sort((a, b) => b.latestSwapBlock - a.latestSwapBlock);
const bufferedEdges = generateBufferedEdges(allPools);
const usedPools = [...allPools.slice(0, POOL_LIMIT), ...ERC4626_VAULTS, ...bufferedEdges];
console.log(`Pools: ${usedPools.length} (limit ${POOL_LIMIT} + ${ERC4626_VAULTS.length} ERC-4626 + ${bufferedEdges.length} buffered)`);

const poolMap = new Map<string, StoredPool>();
for (const p of usedPools) poolMap.set(p.address.toLowerCase(), p);

const rpcUrl = process.env.RPC_URL || "https://api.avax.network/ext/bc/C/rpc";
const wsRpcUrl = process.env.WS_RPC_URL || rpcUrl.replace("http://", "ws://").replace("https://", "wss://").replace("/rpc", "/ws");
const httpClient = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const DUMMY_SENDER = "0x000000000000000000000000000000000000dEaD";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";

// ── Spawn Rust quoter on the captured block ──
const manifestPath = path.join(import.meta.dirname, "../../pathfinders/simple-bfs/Cargo.toml");
const overridesPath = path.join(import.meta.dirname, "../../pathfinders/simple-bfs/data/token_overrides.json");
const stateCacheDir = process.env.STATE_CACHE_DIR
  ? path.resolve(process.env.STATE_CACHE_DIR)
  : path.join(import.meta.dirname, "state_cache");

const proc = spawn(
  "cargo", ["run", "--release", "--manifest-path", manifestPath, "--", "quote", overridesPath, block.toString()],
  {
    env: { ...process.env, WS_RPC_URL: wsRpcUrl, STATE_CACHE_DIR: stateCacheDir },
    stdio: ["pipe", "pipe", "pipe"],
  },
);
proc.stderr!.on("data", (d: Buffer) => process.stderr.write(d));

// ── ndjson protocol ──
let buffer = "";
let nextId = 1;
const pending = new Map<number, { resolve: (v: any) => void; reject: (e: any) => void }>();
const readyPromise = new Promise<any>((resolve) => {
  proc.stdout!.on("data", (d: Buffer) => {
    buffer += d.toString();
    let nl: number;
    while ((nl = buffer.indexOf("\n")) !== -1) {
      const line = buffer.slice(0, nl);
      buffer = buffer.slice(nl + 1);
      if (!line.trim()) continue;
      const msg = JSON.parse(line);
      if (msg.op === "ready") { resolve(msg); continue; }
      const p = pending.get(msg.id);
      if (p) { pending.delete(msg.id); p.resolve(msg); }
    }
  });
});

function send(op: string, params: any): Promise<any> {
  const id = nextId++;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    proc.stdin!.write(JSON.stringify({ op, id, ...params }) + "\n");
  });
}

const ready = await readyPromise;
console.log(`Quoter ready at block ${ready.block}\n`);

const poolsStr = serializePools(headBlock, usedPools);

// ── RPC helpers ──
function buildStateOverrides(tokenIn: string, amountIn: bigint, spender: string) {
  const balOvr = getBalanceOverride(tokenIn, amountIn, DUMMY_SENDER);
  const allowOvr = getAllowanceOverride(tokenIn, DUMMY_SENDER, spender);
  const merged: Record<string, Record<string, Hex>> = {};
  for (const ovr of [balOvr, allowOvr]) {
    for (const [addr, val] of Object.entries(ovr)) {
      if (!merged[addr]) merged[addr] = {};
      Object.assign(merged[addr], val.stateDiff);
    }
  }
  return Object.entries(merged).map(([address, slots]) => ({
    address: address as Hex,
    stateDiff: Object.entries(slots).map(([slot, value]) => ({ slot: slot as Hex, value: value as Hex })),
  }));
}

async function simulateTx(to: string, data: string, tokenIn: string, amountIn: bigint): Promise<bigint | null> {
  try {
    const result = await httpClient.call({
      account: DUMMY_SENDER as Hex,
      to: to as Hex,
      data: data as Hex,
      stateOverride: buildStateOverrides(tokenIn, amountIn, to),
      blockNumber: block,
    });
    if (!result.data || result.data === "0x") return null;
    const [amountOut] = decodeAbiParameters([{ type: "uint256" }], result.data);
    return amountOut;
  } catch { return null; }
}

interface RouteHop { pool: string; pool_type: number; token_in: string; token_out: string; amount_in: string; amount_out: string; extra_data?: string; }

function hopToRouteStep(hop: RouteHop): RouteStep {
  const storedPool = poolMap.get(hop.pool.toLowerCase());
  if (storedPool) return { pool: storedPool, tokenIn: hop.token_in, tokenOut: hop.token_out };
  return {
    pool: { address: hop.pool, poolType: hop.pool_type, tokens: [hop.token_in, hop.token_out], providerName: "", latestSwapBlock: 0, extraData: hop.extra_data } as StoredPool,
    tokenIn: hop.token_in, tokenOut: hop.token_out,
  };
}

async function verifyOurRoute(route: RouteHop[], amountIn: bigint): Promise<bigint | null> {
  if (!route || route.length === 0) return null;
  try {
    return await quoteRoute(httpClient, route.map(hopToRouteStep), amountIn, block);
  } catch { return null; }
}

// ── debug_traceCall to get actual Odos pool addresses ──
interface TraceLog { address: string; topics: string[]; data: string; }
interface CallTrace { from: string; to: string; type: string; input?: string; calls?: CallTrace[]; logs?: TraceLog[]; }

function collectLogs(trace: CallTrace, out: TraceLog[] = []): TraceLog[] {
  if (trace.logs) for (const log of trace.logs) out.push(log);
  if (trace.calls) for (const sub of trace.calls) collectLogs(sub, out);
  return out;
}

const ERC20_TRANSFER = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef";

async function traceOdosTx(txTo: string, txData: string, tokenIn: string, amountIn: bigint): Promise<TraceLog[]> {
  const stateOverrides: Record<string, any> = {};
  const balOvr = getBalanceOverride(tokenIn, amountIn, DUMMY_SENDER);
  const allowOvr = getAllowanceOverride(tokenIn, DUMMY_SENDER, txTo);
  for (const ovr of [balOvr, allowOvr]) {
    for (const [addr, val] of Object.entries(ovr)) {
      if (!stateOverrides[addr]) stateOverrides[addr] = { stateDiff: {} };
      Object.assign(stateOverrides[addr].stateDiff, val.stateDiff);
    }
  }

  try {
    const resp = await httpClient.request({
      method: "debug_traceCall" as any,
      params: [
        { from: DUMMY_SENDER, to: txTo, data: txData, gas: "0x5F5E100" },
        `0x${block.toString(16)}`,
        { tracer: "callTracer", tracerConfig: { withLog: true }, stateOverrides },
      ] as any,
    });
    return collectLogs(resp as CallTrace);
  } catch (e) {
    console.error("  debug_traceCall failed:", (e as Error).message?.slice(0, 200));
    return [];
  }
}

/**
 * Detect pools from ERC20 Transfer events: a pool is any contract address that
 * appears as from/to in transfers of 2+ different tokens.
 * Returns pool addresses with their token sets.
 */
function extractPoolsFromTransfers(logs: TraceLog[]): { address: string; tokens: Set<string> }[] {
  // Map: contract address → set of token addresses that were transferred to/from it
  const tokensByContract = new Map<string, Set<string>>();

  for (const log of logs) {
    if (log.topics[0] !== ERC20_TRANSFER) continue;
    if (log.topics.length < 3) continue;

    const tokenAddr = log.address.toLowerCase();
    const from = ("0x" + log.topics[1].slice(26)).toLowerCase();
    const to = ("0x" + log.topics[2].slice(26)).toLowerCase();

    // Record that this token flowed through 'from' and 'to'
    for (const contract of [from, to]) {
      if (!tokensByContract.has(contract)) tokensByContract.set(contract, new Set());
      tokensByContract.get(contract)!.add(tokenAddr);
    }
  }

  // A pool is any contract with 2+ different tokens
  const result: { address: string; tokens: Set<string> }[] = [];
  for (const [addr, tokens] of tokensByContract) {
    if (tokens.size >= 2) {
      result.push({ address: addr, tokens });
    }
  }

  // Sort by number of tokens descending (most interesting first)
  result.sort((a, b) => b.tokens.size - a.tokens.size);
  return result;
}

// ── Quote all pairs ──
console.log("Quoting all pairs...\n");
const quoteStart = performance.now();

const ourResults = await Promise.all(
  capture.pairs.map((p: any) => send("quote_with_pools", {
    pools: poolsStr,
    token_in: p.token_in,
    token_out: p.token_out,
    amount: p.amount_in,
    input_avax: p.token_in.toLowerCase() === WAVAX ? p.amount_in : "0",
    max_hops: 4,
    gas_limit: 2000000,
  }))
);

const quoteMs = performance.now() - quoteStart;
console.log(`Quoted ${capture.pairs.length} pairs in ${quoteMs.toFixed(0)}ms\n`);

// ── Verify our routes + re-simulate Odos on same block ──
console.log("Verifying on RPC...\n");
const verifyStart = performance.now();

const verifyPromises: Promise<any>[] = [];
for (let i = 0; i < capture.pairs.length; i++) {
  const p = capture.pairs[i];
  const ourRoute: RouteHop[] = ourResults[i].route || [];
  verifyPromises.push(verifyOurRoute(ourRoute, BigInt(p.amount_in)));

  if (p.odos?.tx_to && p.odos?.tx_data) {
    verifyPromises.push(simulateTx(p.odos.tx_to, p.odos.tx_data, p.token_in, BigInt(p.amount_in)));
  } else {
    verifyPromises.push(Promise.resolve(null));
  }
}

const verifyResults = await Promise.all(verifyPromises);
const verifyMs = performance.now() - verifyStart;
console.log(`Verified in ${verifyMs.toFixed(0)}ms\n`);

// ── Print comparison table ──
console.log(`  Block: ${block} | Pools: ${usedPools.length}`);
console.log("");
console.log("  ┌─────────────────────────┬──────────────────┬──────────────────┬──────────────────┬────────┬──────────┐");
console.log("  │ Pair                    │ Ours (RPC)       │ Odos (RPC)       │ Ours (revm)      │ Winner │ Diff %   │");
console.log("  ├─────────────────────────┼──────────────────┼──────────────────┼──────────────────┼────────┼──────────┤");

let wins = 0, losses = 0, ties = 0;
let rpcMatch = 0, rpcMismatch = 0, rpcFail = 0;

for (let i = 0; i < capture.pairs.length; i++) {
  const p = capture.pairs[i];
  const decimalsOut = p.decimals_out;
  const ourRevm = BigInt(ourResults[i].amount_out);
  const ourRpc: bigint | null = verifyResults[i * 2];
  const odosRpc: bigint | null = verifyResults[i * 2 + 1];

  // RPC self-check
  if (ourRpc === null) rpcFail++;
  else if (ourRpc === ourRevm) rpcMatch++;
  else rpcMismatch++;

  const ourBest = ourRpc ?? ourRevm;
  let diff: number | null = null;
  let winner = "—";
  if (ourBest > 0n && odosRpc !== null && odosRpc > 0n) {
    diff = Number(ourBest - odosRpc) / Number(odosRpc) * 100;
    if (ourBest > odosRpc) { winner = "Ours"; wins++; }
    else if (odosRpc > ourBest) { winner = "Odos"; losses++; }
    else { winner = "Tie"; ties++; }
  }

  const pairCol = p.name.padEnd(23);
  const ourRpcStr = ourRpc !== null ? formatUnits(ourRpc, decimalsOut).slice(0, 16) : "FAIL";
  const rpcFlag = ourRpc === null ? "" : (ourRpc === ourRevm ? " ✓" : " ✗");
  const odosStr = odosRpc !== null ? formatUnits(odosRpc, decimalsOut).slice(0, 16) : "FAIL";
  const revmStr = formatUnits(ourRevm, decimalsOut).slice(0, 16);
  const diffStr = diff !== null ? `${diff >= 0 ? "+" : ""}${diff.toFixed(3)}%` : "—";

  console.log(`  │ ${pairCol} │ ${(ourRpcStr + rpcFlag).padEnd(16)} │ ${odosStr.padEnd(16)} │ ${revmStr.padEnd(16)} │ ${winner.padEnd(6)} │ ${diffStr.padEnd(8)} │`);
}

console.log("  └─────────────────────────┴──────────────────┴──────────────────┴──────────────────┴────────┴──────────┘");

const winRate = wins + losses > 0 ? (wins / (wins + losses) * 100).toFixed(1) : "N/A";
console.log(`\n  RPC self-check: ${rpcMatch}✓ ${rpcMismatch}✗ ${rpcFail}?  |  vs Odos: ${wins}W ${losses}L ${ties}T  |  Win rate: ${winRate}%`);

// ── Detail + trace for losing/close pairs ──
const interestingPairs: number[] = [];
for (let i = 0; i < capture.pairs.length; i++) {
  const p = capture.pairs[i];
  const ourRevm = BigInt(ourResults[i].amount_out);
  const ourRpc: bigint | null = verifyResults[i * 2];
  const odosRpc: bigint | null = verifyResults[i * 2 + 1];
  const ourBest = ourRpc ?? ourRevm;
  if (odosRpc === null || odosRpc <= 0n) continue;
  const diff = Number(ourBest - odosRpc) / Number(odosRpc) * 100;
  if (diff < 0.15) interestingPairs.push(i);
}

if (interestingPairs.length > 0) {
  console.log(`\n── Tracing ${interestingPairs.length} interesting pairs via debug_traceCall ──`);
}

for (const i of interestingPairs) {
  const p = capture.pairs[i];
  const ourRevm = BigInt(ourResults[i].amount_out);
  const ourRpc: bigint | null = verifyResults[i * 2];
  const odosRpc: bigint | null = verifyResults[i * 2 + 1];
  const ourBest = ourRpc ?? ourRevm;
  const diff = Number(ourBest - odosRpc!) / Number(odosRpc!) * 100;

  console.log(`\n  ── ${p.name} (${diff >= 0 ? "+" : ""}${diff.toFixed(4)}%)  [${ourBest > odosRpc! ? "Ours" : ourBest < odosRpc! ? "Odos" : "Tie"}] ──`);

  // Our route
  const ourRoute: RouteHop[] = ourResults[i].route || [];
  if (ourRoute.length > 0) {
    const r = ourResults[i];
    console.log(`    Our route (${r.hops} hops, ${r.evm_calls} evm, ${r.requote_calls} requotes, ${r.elapsed_ms}ms):`);
    for (const hop of ourRoute) {
      const known = poolMap.get(hop.pool.toLowerCase());
      const provider = known ? known.providerName : "???";
      console.log(`      ${hop.token_in.slice(0, 10)}→${hop.token_out.slice(0, 10)} via ${hop.pool} (${provider})`);
    }
  }

  // Odos pathViz (self-reported, for reference)
  if (p.odos?.path_viz?.links) {
    console.log(`    Odos route (self-reported):`);
    for (const link of p.odos.path_viz.links) {
      const src = link.sourceToken?.symbol || "?";
      const tgt = link.targetToken?.symbol || "?";
      const pct = typeof link.value === "number" ? `${link.value.toFixed(0)}%` : "";
      console.log(`      ${src}→${tgt} via "${link.label}" ${pct}`);
    }
  }

  // Trace Odos tx on-chain — detect pools by token flow
  if (p.odos?.tx_to && p.odos?.tx_data) {
    console.log(`    Odos pools (from ERC20 transfer trace):`);
    const logs = await traceOdosTx(p.odos.tx_to, p.odos.tx_data, p.token_in, BigInt(p.amount_in));
    const pools = extractPoolsFromTransfers(logs);

    if (pools.length === 0) {
      console.log(`      (no multi-token contracts found)`);
    } else {
      for (const pool of pools) {
        const known = poolMap.get(pool.address);
        const tag = known
          ? `✓ (${known.providerName})`
          : `✗ MISSING`;
        const tokenList = [...pool.tokens].map(t => t.slice(0, 10)).join(", ");
        console.log(`      ${pool.address} [${pool.tokens.size} tokens: ${tokenList}] ${tag}`);
      }
    }
  }
}

console.log("");
proc.stdin!.end();
proc.kill();
process.exit(0);
