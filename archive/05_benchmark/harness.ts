// Benchmark harness: runs pathfinders against captured Odos ground truth.
//
// Usage:
//   node harness.ts                              # run all captures in captures/
//   node harness.ts captures/80363980.json        # run single capture
//   node harness.ts captures/a.json captures/b.json  # run specific captures

import { spawn, execSync, type ChildProcess } from "node:child_process";
import * as fs from "node:fs";
import path from "node:path";
import { createPublicClient, http, formatUnits, type Hex, encodeFunctionData, decodeAbiParameters } from "viem";
import { avalanche } from "viem/chains";
import { loadDotEnv } from "../../utils/env.ts";
import {
  loadPools, defaultPoolsPath, serializePools, ERC4626_VAULTS, generateBufferedEdges,
} from "../../pools/index.ts";
import { getBalanceOverride } from "../../router/overrides.ts";

loadDotEnv();

const ROOT = path.join(import.meta.dirname, "../..");
const PROXY_DIR = path.join(ROOT, "tools/state-proxy");
const CAPTURES_DIR = path.join(import.meta.dirname, "captures");
const PROXY_CACHE_DIR = path.join(import.meta.dirname, "proxy_cache");

const rpcUrl = process.env.RPC_URL || "http://localhost:9650/ext/bc/C/rpc";
const wsRpcUrl = process.env.WS_RPC_URL || rpcUrl.replace("http://", "ws://").replace("https://", "wss://").replace("/rpc", "/ws");

const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const PROXY_WS = "ws://127.0.0.1:8547/ws";

// Pathfinder selection: "rust" (default) or "go"
const PATHFINDER = (process.env.PATHFINDER || "rust").toLowerCase();

// ── Capture format ──
interface CapturedPair {
  name: string;
  token_in: string;
  token_out: string;
  amount_in: string;
  decimals_in: number;
  decimals_out: number;
  odos_tx_to: string | null;
  odos_tx_data: string | null;
  odos_simulated_out: string | null;
}

interface Capture {
  block: number;
  timestamp: string;
  pairs: CapturedPair[];
}

// ── Discover capture files ──
let captureFiles: string[];
if (process.argv.length > 2) {
  captureFiles = process.argv.slice(2);
} else {
  // Load all captures sorted by block number
  captureFiles = fs.readdirSync(CAPTURES_DIR)
    .filter(f => f.endsWith(".json"))
    .sort()
    .map(f => path.join(CAPTURES_DIR, f));
}

if (captureFiles.length === 0) {
  console.error("No captures found. Run: node capture.ts");
  process.exit(1);
}

console.log(`Found ${captureFiles.length} capture(s)\n`);

// ── Load pools (once, shared across all blocks) ──
const POOL_LIMIT = parseInt(process.env.POOL_LIMIT || "7500");
const { pools, headBlock } = loadPools(defaultPoolsPath());
const allPools = [...pools.values()];
allPools.sort((a, b) => b.latestSwapBlock - a.latestSwapBlock);
const bufferedEdges = generateBufferedEdges(allPools);
const usedPools = [...allPools.slice(0, POOL_LIMIT), ...ERC4626_VAULTS, ...bufferedEdges];
const poolsStr = serializePools(headBlock, usedPools);
console.log(`Pools: ${usedPools.length}`);
console.log(`Pathfinder: ${PATHFINDER}\n`);

const httpClient = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

interface RouteHop { pool: string; pool_type: number; token_in: string; token_out: string; amount_in: string; amount_out: string; extra_data?: string; }

// MultiQuoter contract address and bytecode — same as used by local BFS
const QUOTER_ADDR = "0x00000000000000000000000000000000DeaDBeef" as Hex;
const DUMMY_SENDER = "0x000000000000000000000000000000000000dEaD" as Hex;
const quoterBytecodeHex = fs.readFileSync(path.join(ROOT, "pathfinders/simple-bfs/data/quoter_bytecode.hex"), "utf-8").trim();
const quoterBytecode = (quoterBytecodeHex.startsWith("0x") ? quoterBytecodeHex : `0x${quoterBytecodeHex}`) as Hex;

// MultiQuoter's swap ABI: swap(address[],uint8[],address[],uint256) returns (uint256)
const multiQuoterSwapAbi = [{
  name: "swap", type: "function",
  inputs: [
    { name: "pools", type: "address[]" },
    { name: "poolTypes", type: "uint8[]" },
    { name: "tokens", type: "address[]" },
    { name: "amountIn", type: "uint256" },
  ],
  outputs: [{ type: "uint256" }],
}] as const;

async function verifyRoute(route: RouteHop[], amountIn: bigint, block: bigint): Promise<bigint | null> {
  if (!route || route.length === 0) return null;
  try {
    const pools = route.map(h => h.pool as Hex);
    const poolTypes = route.map(h => h.pool_type);
    const tokens = [route[0].token_in as Hex, ...route.map(h => h.token_out as Hex)];

    const calldata = encodeFunctionData({
      abi: multiQuoterSwapAbi,
      functionName: "swap",
      args: [pools, poolTypes, tokens, amountIn],
    });

    const inputToken = route[0].token_in.toLowerCase();
    // Give the QUOTER contract the input token balance (MultiQuoter holds tokens directly)
    const balanceOverride = getBalanceOverride(inputToken, amountIn * 1000n, QUOTER_ADDR.toLowerCase());

    // Build state override array for viem
    const stateOverrideArray: any[] = [
      // Inject MultiQuoter bytecode
      { address: QUOTER_ADDR, code: quoterBytecode },
    ];
    // Add token balance overrides
    for (const [addr, val] of Object.entries(balanceOverride)) {
      stateOverrideArray.push({
        address: addr as Hex,
        stateDiff: Object.entries(val.stateDiff).map(([slot, value]) => ({ slot: slot as Hex, value: value as Hex })),
      });
    }

    const result = await httpClient.call({
      account: DUMMY_SENDER,
      to: QUOTER_ADDR,
      data: calldata,
      stateOverride: stateOverrideArray,
      blockNumber: block,
    });

    if (!result.data || result.data === "0x") return null;
    const [amountOut] = decodeAbiParameters([{ type: "uint256" }], result.data);
    return amountOut;
  } catch (e: any) {
    const poolsDesc = route.map(h => `${h.pool}(type=${h.pool_type})`).join(" → ");
    console.error(`    [rpc-verify] FAILED: ${e.shortMessage || e.message}\n      route: ${poolsDesc}\n      ${e.details || ''}`);
    if (e.cause) console.error(`      cause: ${e.cause.message || e.cause}`);
    return null;
  }
}

// ── Kill any leftover proxy processes (avoid matching our own node process) ──
try { execSync("pkill -x state-proxy", { stdio: "pipe" }); } catch {}
await new Promise(r => setTimeout(r, 300));

// ── Build proxy once ──
console.log("Building state-proxy...");
execSync("go build -o state-proxy .", { cwd: PROXY_DIR, stdio: "pipe" });

// ── Proxy management ──
function startProxy(): { proc: ChildProcess; ready: Promise<void> } {
  const proc = spawn(path.join(PROXY_DIR, "state-proxy"), [
    "-port", "8547",
    "-upstream", wsRpcUrl,
    "-cache", PROXY_CACHE_DIR,
  ], { stdio: ["pipe", "pipe", "pipe"] });

  let readyResolve: () => void;
  const ready = new Promise<void>(r => { readyResolve = r; });
  let resolved = false;

  proc.stderr!.on("data", (d: Buffer) => {
    for (const line of d.toString().trim().split("\n")) {
      if (line) console.log(`  [proxy] ${line}`);
      if (!resolved && (line.includes("state-proxy ws://") || line.includes("listening"))) {
        resolved = true;
        readyResolve();
      }
    }
  });

  // Fallback: resolve after 1s if no "listening" message
  setTimeout(() => { if (!resolved) { resolved = true; readyResolve(); } }, 1000);

  return { proc, ready };
}

async function stopProxy(proc: ChildProcess) {
  proc.kill("SIGINT");
  await new Promise<void>(resolve => proc.on("close", () => resolve()));
}

// ── BFS quoter ──
function spawnBfsQuoter(block: number, blockHex: string): {
  proc: ChildProcess;
  send: (op: string, params: any) => Promise<any>;
  ready: Promise<any>;
} {
  const overridesPath = path.join(ROOT, "pathfinders/simple-bfs/data/token_overrides.json");
  let proc: ChildProcess;

  if (PATHFINDER === "go") {
    // Go pathfinder — connects to proxy via WebSocket (same as Rust)
    const goBinary = path.join(ROOT, "archive/quoting-go/quoting-go");
    const env: Record<string, string> = { ...process.env as any, WS_RPC_URL: PROXY_WS };
    proc = spawn(goBinary, ["quote", overridesPath, block.toString()],
      { env, cwd: path.join(ROOT, "archive/quoting-go"), stdio: ["pipe", "pipe", "pipe"] });
  } else {
    // Rust pathfinder — build once, then run binary directly
    const bfsDir = path.join(ROOT, "pathfinders/simple-bfs");
    const bfsBinary = path.join(bfsDir, "target/release/quote");
    if (!fs.existsSync(bfsBinary)) {
      console.log("Building simple-bfs (first time)...");
      execSync("cargo build --release", { cwd: bfsDir, stdio: "inherit" });
    }
    const proxyCachePath = path.join(PROXY_CACHE_DIR, blockHex + ".json");
    const env: Record<string, string> = { ...process.env as any, WS_RPC_URL: PROXY_WS };
    if (fs.existsSync(proxyCachePath)) {
      env.PROXY_CACHE = proxyCachePath;
    }
    delete env.STATE_CACHE_DIR;
    proc = spawn(bfsBinary, ["quote", overridesPath, block.toString()],
      { env, stdio: ["pipe", "pipe", "pipe"] });
  }
  proc.stderr!.on("data", (d: Buffer) => {
    for (const line of d.toString().trim().split("\n")) {
      if (line) console.log(`  [bfs] ${line}`);
    }
  });

  let buffer = "";
  let nextId = 1;
  const pending = new Map<number, { resolve: (v: any) => void; reject: (e: any) => void }>();

  const ready = new Promise<any>((resolve) => {
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

  return { proc, send, ready };
}

function killQuoter(quoter: { proc: ChildProcess }) {
  quoter.proc.stdin!.end();
  quoter.proc.kill();
}

// ── Run BFS on one block ──
async function runBfsOnBlock(capture: Capture, blockHex: string): Promise<{ results: any[]; measuredMs: number }> {
  const quoter = spawnBfsQuoter(capture.block, blockHex);
  const readyMsg = await quoter.ready;
  console.log(`  Quoter ready (block ${readyMsg.block})`);

  const QUOTE_TIMEOUT_MS = 180_000; // 3 minute hard timeout

  function withTimeout<T>(promise: Promise<T>, ms: number, label: string): Promise<T> {
    return Promise.race([
      promise,
      new Promise<T>((_, reject) => setTimeout(() => reject(new Error(`TIMEOUT: ${label} after ${ms/1000}s`)), ms)),
    ]);
  }

  console.log("  Quoting...");
  const start = performance.now();
  const results = await withTimeout(Promise.all(
    capture.pairs.map(p => quoter.send("quote_with_pools", {
      pools: poolsStr,
      token_in: p.token_in,
      token_out: p.token_out,
      amount: p.amount_in,
      input_avax: p.token_in.toLowerCase() === WAVAX ? p.amount_in : "0",
      max_hops: 3,
      gas_limit: 2000000,
    }))
  ), QUOTE_TIMEOUT_MS, "measured run");
  const measuredMs = performance.now() - start;

  killQuoter(quoter);
  return { results, measuredMs };
}

// ── Verify and print results for one block ──
async function verifyAndPrintResults(capture: Capture, bfsResults: any[]): Promise<{ wins: number; losses: number; ties: number }> {
  const block = BigInt(capture.block);

  // Verify all BFS routes via direct RPC (independent of REVM)
  console.log("  Verifying BFS routes via RPC...");
  const rpcResults = await Promise.all(
    capture.pairs.map((p, i) => {
      const route: RouteHop[] = bfsResults[i].route || [];
      return verifyRoute(route, BigInt(p.amount_in), block);
    })
  );

  console.log("");
  console.log("  ┌─────────────────────────┬──────────────────┬──────────────────┬────────┬──────────┬──────────┬───────┐");
  console.log("  │ Pair                    │ BFS (revm)       │ Odos (RPC)       │ Winner │ Diff %   │ BFS ms   │ RPC?  │");
  console.log("  ├─────────────────────────┼──────────────────┼──────────────────┼────────┼──────────┼──────────┼───────┤");

  let wins = 0, losses = 0, ties = 0;
  let rpcMatch = 0, rpcMismatch = 0;

  for (let i = 0; i < capture.pairs.length; i++) {
    const p = capture.pairs[i];
    const bfsRevm = BigInt(bfsResults[i].amount_out);
    const bfsRpc = rpcResults[i];
    const odosOut = p.odos_simulated_out ? BigInt(p.odos_simulated_out) : null;

    // RPC parity check — FAIL counts as mismatch
    let rpcFlag: string;
    if (bfsRpc === null) { rpcFlag = "FAIL"; rpcMismatch++; }
    else if (bfsRpc === bfsRevm) { rpcFlag = "  ✓  "; rpcMatch++; }
    else { rpcFlag = "  ✗  "; rpcMismatch++; }

    // Use RPC result if available for comparison (it's ground truth)
    const bfsBest = bfsRpc ?? bfsRevm;

    let diff: number | null = null;
    let winner = "—";
    if (bfsBest > 0n && odosOut !== null && odosOut > 0n) {
      diff = Number(bfsBest - odosOut) / Number(odosOut) * 100;
      if (bfsBest > odosOut) { winner = "BFS"; wins++; }
      else if (odosOut > bfsBest) { winner = "Odos"; losses++; }
      else { winner = "Tie"; ties++; }
    }

    const pairCol = p.name.padEnd(23);
    const bfsStr = formatUnits(bfsRevm, p.decimals_out).slice(0, 16).padEnd(16);
    const odosStr = odosOut !== null ? formatUnits(odosOut, p.decimals_out).slice(0, 16).padEnd(16) : "FAIL".padEnd(16);
    const diffStr = diff !== null ? `${diff >= 0 ? "+" : ""}${diff.toFixed(3)}%` : "—";
    const bfsTimeStr = `${bfsResults[i].elapsed_ms}ms`;

    console.log(`  │ ${pairCol} │ ${bfsStr} │ ${odosStr} │ ${winner.padEnd(6)} │ ${diffStr.padEnd(8)} │ ${bfsTimeStr.padEnd(8)} │${rpcFlag} │`);
  }

  console.log("  └─────────────────────────┴──────────────────┴──────────────────┴────────┴──────────┴──────────┴───────┘");

  if (rpcMismatch > 0) {
    console.log(`\n  🚨 REVM PARITY: ${rpcMatch}✓ ${rpcMismatch}✗ — ${rpcMismatch} FAILURES`);
    for (let i = 0; i < capture.pairs.length; i++) {
      const bfsRevm = BigInt(bfsResults[i].amount_out);
      const bfsRpc = rpcResults[i];
      const p = capture.pairs[i];
      if (bfsRpc === null) {
        console.log(`    ${p.name}: RPC call failed (revm=${bfsRevm})`);
      } else if (bfsRpc !== bfsRevm) {
        const delta = bfsRevm > bfsRpc ? bfsRevm - bfsRpc : bfsRpc - bfsRevm;
        console.log(`    ${p.name}: revm=${bfsRevm} rpc=${bfsRpc} delta=${delta} wei`);
      }
    }
  } else {
    console.log(`\n  ✅ REVM parity: ${rpcMatch}/${capture.pairs.length} verified`);
  }

  return { wins, losses, ties };
}

// ── Cleanup on unexpected exit ──
function cleanup() {
  try { execSync("pkill -x state-proxy", { stdio: "pipe" }); } catch {}
}
process.on("SIGINT", () => { cleanup(); process.exit(130); });
process.on("SIGTERM", () => { cleanup(); process.exit(143); });

// ── Main: start proxy once, loop through all captures ──
const proxy = startProxy();
await proxy.ready;

let totalWins = 0, totalLosses = 0, totalTies = 0;

for (const captureFile of captureFiles) {
  const capture: Capture = JSON.parse(fs.readFileSync(captureFile, "utf-8"));
  const blockHex = "0x" + capture.block.toString(16);

  console.log(`\n${"═".repeat(70)}`);
  console.log(`Block ${capture.block} (${capture.timestamp})`);
  console.log(`${"═".repeat(70)}\n`);

  // Run BFS (fresh process per block)
  const { results, measuredMs } = await runBfsOnBlock(capture, blockHex);

  // Verify & print results (RPC verification goes directly to node, not proxy)
  const { wins, losses, ties } = await verifyAndPrintResults(capture, results);
  const total = wins + losses;
  const winRate = total > 0 ? (wins / total * 100).toFixed(1) : "N/A";
  console.log(`\n  Block ${capture.block}: ${wins}W ${losses}L ${ties}T | Win rate: ${winRate}% | Measured: ${measuredMs.toFixed(0)}ms`);

  totalWins += wins;
  totalLosses += losses;
  totalTies += ties;
}

// Stop proxy (saves all block caches)
await stopProxy(proxy.proc);

// ── Summary ──
const grandTotal = totalWins + totalLosses;
const grandWinRate = grandTotal > 0 ? (totalWins / grandTotal * 100).toFixed(1) : "N/A";

console.log(`\n${"═".repeat(70)}`);
console.log(`TOTAL across ${captureFiles.length} blocks: ${totalWins}W ${totalLosses}L ${totalTies}T | Win rate: ${grandWinRate}%`);
console.log(`${"═".repeat(70)}`);

process.exit(0);
