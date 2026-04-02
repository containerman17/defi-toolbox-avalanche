// WASM EVM eth_call correctness test: local WASM EVM vs public RPC node.
//
// Calls LFJ LBQuoter.findBestPathFromAmountIn([WAVAX, USDC], 1 AVAX)
// every block. Compares at the SAME block number (not "latest").
//
// On block notification: immediately queries WASM EVM, stores result.
// Main loop: waits 4s for public node to catch up, then queries with
// explicit block number. Retries up to 3 times. Exits immediately on
// any wei-level mismatch.
//
// Usage: node run.mjs [stateServerUrl] [durationSec]
//
// Requires: state-server running at ws://localhost:7449/live
//           ethcall.wasm + wasm_exec.js built in this directory
//           Build: make build-wasm-ethcall

import { readFile } from "fs/promises";
import { argv } from "process";

// ── Config ──────────────────────────────────────────────────────────

const STATE_SERVER = argv[2] || "ws://localhost:7449/live";
const DURATION_SEC = parseInt(argv[3] || "180");
const PUBLIC_RPC = "https://api.avax.network/ext/bc/C/rpc";
const SETTLE_DELAY_MS = 4000; // wait for public node to reach the block
const MAX_RETRIES = 3;

// Addresses
const WAVAX = "0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7";
const USDC  = "0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E";
const LB_QUOTER = "0x9A550a522BBaDFB69019b0432800Ed17855A51C3"; // LBQuoter V2.2

// ── ABI encoding ────────────────────────────────────────────────────

function encodeLBQuoterCall(tokenIn, tokenOut, amountIn) {
  const selector = "0f902a40";
  const offset = "0000000000000000000000000000000000000000000000000000000000000040";
  const amount = BigInt(amountIn).toString(16).padStart(64, "0");
  const len    = "0000000000000000000000000000000000000000000000000000000000000002";
  const addr1  = tokenIn.slice(2).toLowerCase().padStart(64, "0");
  const addr2  = tokenOut.slice(2).toLowerCase().padStart(64, "0");
  return "0x" + selector + offset + amount + len + addr1 + addr2;
}

function decodeAmountOut(resultHex) {
  const data = resultHex.slice(2);
  if (data.length < 64 * 8) return "0";
  const tupleStart = 1;
  const amountsRelOffset = parseInt(data.slice((tupleStart + 4) * 64, (tupleStart + 5) * 64), 16);
  const amountsAbs = (tupleStart * 32 + amountsRelOffset) * 2;
  const amountsLen = parseInt(data.slice(amountsAbs, amountsAbs + 64), 16);
  if (amountsLen < 2) return "0";
  const lastIdx = amountsLen - 1;
  const lastWord = data.slice(amountsAbs + 64 + lastIdx * 64, amountsAbs + 128 + lastIdx * 64);
  return BigInt("0x" + lastWord).toString();
}

// ── Load WASM ───────────────────────────────────────────────────────

const execJs = await readFile(new URL("./wasm_exec.js", import.meta.url), "utf8");
new Function(execJs)();

const go = new Go();
console.log("loading wasm...");
const wasmBytes = await readFile(new URL("./ethcall.wasm", import.meta.url));
const { instance } = await WebAssembly.instantiate(wasmBytes, go.importObject);
go.run(instance);
await new Promise((r) => setTimeout(r, 100));

// ── Connect to state server ────────────────────────────────────────

console.log(`connecting to ${STATE_SERVER}...`);
const t0 = Date.now();
await globalThis.connect(STATE_SERVER);
console.log(`connected in ${Date.now() - t0}ms\n`);

// ── Build calldata ──────────────────────────────────────────────────

const ONE_AVAX = "1000000000000000000";
const calldata = encodeLBQuoterCall(WAVAX, USDC, ONE_AVAX);
console.log(`LBQuoter V2.2: ${LB_QUOTER}`);
console.log(`call: findBestPathFromAmountIn([WAVAX, USDC], 1 AVAX)\n`);

// ── Helper: eth_call via public RPC at specific block ───────────────

async function publicEthCallAtBlock(to, data, blockNum) {
  const blockHex = "0x" + blockNum.toString(16);
  const body = JSON.stringify({
    jsonrpc: "2.0",
    id: 1,
    method: "eth_call",
    params: [{ to, data }, blockHex],
  });
  const t = performance.now();
  const resp = await fetch(PUBLIC_RPC, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body,
  });
  const elapsed = performance.now() - t;
  const json = await resp.json();
  if (json.error) {
    return { result: null, ms: elapsed, error: json.error.message };
  }
  return { result: json.result, ms: elapsed };
}

// ── Warmup ──────────────────────────────────────────────────────────

console.log("=== warmup ===");
const warmLocal = await globalThis.ethCall(LB_QUOTER, calldata);
if (warmLocal.error) {
  console.log(`  WASM EVM:    ${warmLocal.ms.toFixed(2)}ms  ERROR: ${warmLocal.error}`);
} else {
  console.log(`  WASM EVM:    ${warmLocal.ms.toFixed(2)}ms  gas=${warmLocal.gasUsed}  out=${decodeAmountOut(warmLocal.result)} wei`);
}
console.log();

// ── Block notification → immediate local query ──────────────────────

// Queue of { block, wasmResult, wasmMs } entries waiting for RPC verification
const pendingQueue = [];

globalThis.subscribeBlocks((block, timestamp) => {
  // Query WASM EVM immediately on block notification
  globalThis.ethCall(LB_QUOTER, calldata).then((result) => {
    pendingQueue.push({ block, wasmResult: result, wasmMs: result.ms });
  });
});

// ── Main loop: verify against public RPC at same block ──────────────

console.log(`=== correctness test for ${DURATION_SEC}s (${SETTLE_DELAY_MS}ms settle delay) ===`);
console.log(
  `${"block".padEnd(12)} ` +
  `${"WASM".padEnd(10)} ` +
  `${"RPC".padEnd(10)} ` +
  `${"WASM wei".padEnd(16)} ` +
  `${"RPC wei".padEnd(16)} ` +
  `result`
);
console.log("-".repeat(78));

let lastWasmAmt = "";
let totalVerified = 0;
let changes = 0;
const wasmTimes = [];
const rpcTimes = [];

const deadline = Date.now() + DURATION_SEC * 1000;

while (Date.now() < deadline) {
  // Wait for a pending entry
  while (pendingQueue.length === 0) {
    if (Date.now() >= deadline) break;
    await new Promise((r) => setTimeout(r, 10));
  }
  if (Date.now() >= deadline) break;

  const entry = pendingQueue.shift();

  if (entry.wasmResult.error) {
    console.log(`${String(entry.block).padEnd(12)}  WASM ERROR: ${entry.wasmResult.error}`);
    continue;
  }

  const wasmAmt = decodeAmountOut(entry.wasmResult.result);

  // Wait for public node to reach this block
  await new Promise((r) => setTimeout(r, SETTLE_DELAY_MS));

  // Query public RPC at the exact same block number, with retries
  let rpcAmt = null;
  let rpcMs = 0;
  let rpcError = null;

  for (let attempt = 0; attempt < MAX_RETRIES; attempt++) {
    const remote = await publicEthCallAtBlock(LB_QUOTER, calldata, entry.block);
    if (remote.error) {
      rpcError = remote.error;
      // Block might not be available yet, wait and retry
      await new Promise((r) => setTimeout(r, 2000));
      continue;
    }
    rpcAmt = decodeAmountOut(remote.result);
    rpcMs = remote.ms;
    rpcError = null;
    break;
  }

  if (rpcError) {
    console.log(`${String(entry.block).padEnd(12)}  RPC ERROR after ${MAX_RETRIES} retries: ${rpcError}`);
    continue;
  }

  totalVerified++;
  const changed = wasmAmt !== lastWasmAmt && lastWasmAmt !== "";
  if (changed) changes++;
  lastWasmAmt = wasmAmt;

  wasmTimes.push(entry.wasmMs);
  rpcTimes.push(rpcMs);

  const match = wasmAmt === rpcAmt;
  const changeMarker = changed ? " ◀ CHANGED" : "";

  console.log(
    `${String(entry.block).padEnd(12)} ` +
    `${(entry.wasmMs.toFixed(2) + "ms").padStart(8)}  ` +
    `${(rpcMs.toFixed(2) + "ms").padStart(8)}  ` +
    `${wasmAmt.padStart(14)}  ` +
    `${rpcAmt.padStart(14)}  ` +
    `${match ? "✓" : "MISMATCH"}${changeMarker}`
  );

  if (!match) {
    console.error(`\n!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!`);
    console.error(`!!! WEI MISMATCH at block ${entry.block} !!!`);
    console.error(`!!!   WASM: ${wasmAmt}`);
    console.error(`!!!   RPC:  ${rpcAmt}`);
    console.error(`!!!   diff: ${BigInt(wasmAmt) - BigInt(rpcAmt)} wei`);
    console.error(`!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!`);
    process.exit(1);
  }
}

// ── Summary ─────────────────────────────────────────────────────────

const avgWasm = wasmTimes.length ? wasmTimes.reduce((a, b) => a + b, 0) / wasmTimes.length : 0;
const avgRpc = rpcTimes.length ? rpcTimes.reduce((a, b) => a + b, 0) / rpcTimes.length : 0;

console.log("\n=== summary ===");
console.log(`  verified:   ${totalVerified} blocks (same block number, wei-exact)`);
console.log(`  changes:    ${changes} (quote value changed between blocks)`);
console.log(`  mismatches: 0`);
console.log(`  WASM EVM    avg: ${avgWasm.toFixed(2)}ms`);
console.log(`  public RPC  avg: ${avgRpc.toFixed(2)}ms`);
if (avgWasm > 0) console.log(`  avg speedup: ${(avgRpc / avgWasm).toFixed(1)}x`);

process.exit(0);
