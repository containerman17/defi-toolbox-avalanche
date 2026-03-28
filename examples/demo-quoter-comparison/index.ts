// Demo: Compare Native (EVM+Formula), Native (Formula-only), and Wasm (Formula-only)
//
// Usage: node examples/demo-quoter-comparison/index.ts

import { createQuoter, TOKENS } from "../../packages/hayabusa/dist/index.js";
import { readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { WebSocket } from "../../packages/hayabusa/node_modules/ws/wrapper.mjs";

const __dirname = dirname(fileURLToPath(import.meta.url));
const BIN = join(__dirname, "../../packages/hayabusa/bin");
const STATE_SERVER_URL = "ws://localhost:7449/live";

const WAVAX = TOKENS.WAVAX;
const USDC = TOKENS.USDC;
const ONE_AVAX = 10n ** 18n;

// ── Wasm runner ──────────────────────────────────────────────────────

async function runWasm(): Promise<{ amountOut: string; stats: any; warmMs: number; hotMs: number }> {
  // Load Go WASM runtime
  const wasmExecJs = readFileSync(join(BIN, "wasm_exec.js"), "utf-8");
  // Execute wasm_exec.js to get globalThis.Go
  const fn = new Function(wasmExecJs);
  fn();

  const GoClass = (globalThis as any).Go;
  const go = new GoClass();

  // Load and instantiate WASM
  const wasmBuf = readFileSync(join(BIN, "harness.wasm"));
  const { instance } = await WebAssembly.instantiate(wasmBuf, go.importObject);
  go.run(instance);

  const g = globalThis as any;

  // Wait for Go to be ready
  let tries = 0;
  while (!g.__goReady) {
    if (++tries > 500) throw new Error("WASM: __goReady not set after 5s");
    await new Promise(r => setTimeout(r, 10));
  }

  // Connect to state server and prefill state
  const ws = await new Promise<WebSocket>((resolve, reject) => {
    const w = new WebSocket(STATE_SERVER_URL);
    w.on("error", reject);
    w.on("open", () => resolve(w));
  });

  // Read initial_dump
  const dump: any = await new Promise((resolve) => {
    ws.on("message", function handler(data: any) {
      const msg = JSON.parse(data.toString());
      if (msg.type === "initial_dump") {
        ws.off("message", handler);
        resolve(msg);
      }
    });
  });

  g.__goSetBlock(dump.blockNumber, dump.timestamp || 0, dump.baseFee || 0, dump.gasLimit || 0);

  // Prefill storage and accounts
  const acctData = new Map<string, any>();
  let storageCount = 0;
  for (const [key, value] of dump.entries) {
    if (key.startsWith("s:")) {
      const parts = key.split(":");
      if (parts.length === 3) {
        g.__goPrefillStorage(parts[1], parts[2], value);
        storageCount++;
      }
    } else if (key.startsWith("b:")) {
      const addr = key.slice(2);
      if (!acctData.has(addr)) acctData.set(addr, {});
      acctData.get(addr).balance = value;
    } else if (key.startsWith("n:")) {
      const addr = key.slice(2);
      if (!acctData.has(addr)) acctData.set(addr, {});
      acctData.get(addr).nonce = value;
    } else if (key.startsWith("c:")) {
      const addr = key.slice(2);
      if (!acctData.has(addr)) acctData.set(addr, {});
      acctData.get(addr).code = value;
    }
  }
  for (const [addr, d] of acctData) {
    g.__goPrefillAccount(addr, d.balance || "0x0", d.nonce ? parseInt(d.nonce, 16) : 0, d.code || "0x");
  }

  console.error(`[wasm] state loaded: block=${dump.blockNumber}, ${storageCount} storage, ${acctData.size} accounts`);

  // Register fetch callbacks (needed even if not used, Go calls them)
  const ssReqId = { v: 0 };
  function stateServerRequest(method: string, params: any): Promise<string> {
    return new Promise((resolve, reject) => {
      const id = ++ssReqId.v;
      const handler = (data: any) => {
        const msg = JSON.parse(data.toString());
        if (msg.id !== id) return;
        ws.off("message", handler);
        if (msg.error) reject(new Error(JSON.stringify(msg.error)));
        else resolve(msg.result.value);
      };
      ws.on("message", handler);
      ws.send(JSON.stringify({ jsonrpc: "2.0", id, method, params }));
    });
  }

  g.__goFetchStorageAsync = (addr: string, slot: string) =>
    stateServerRequest("state_getStorageAt", { address: addr, slot, blockNumber: dump.blockNumber });
  g.__goFetchBalanceAsync = (addr: string) =>
    stateServerRequest("state_getBalance", { address: addr, blockNumber: dump.blockNumber });
  g.__goFetchNonceAsync = (addr: string) =>
    stateServerRequest("state_getNonce", { address: addr, blockNumber: dump.blockNumber });
  g.__goFetchCodeAsync = (addr: string) =>
    stateServerRequest("state_getCode", { address: addr, blockNumber: dump.blockNumber });
  g.__goFetchBlockHashAsync = () => Promise.resolve("0x" + "0".repeat(64));

  // Helper to call __goFindRoute
  function findRoute(formulaOnly: boolean): Promise<any> {
    return new Promise((resolve) => {
      g.__goFindRoute(
        WAVAX, USDC,
        "0x" + ONE_AVAX.toString(16),
        2, formulaOnly,
        (resultJson: string) => resolve(JSON.parse(resultJson)),
      );
    });
  }

  // Warm-up run
  const tw0 = performance.now();
  await findRoute(true);
  const warmMs = performance.now() - tw0;

  // Timed run
  const t0 = performance.now();
  const result = await findRoute(true);
  const hotMs = performance.now() - t0;

  ws.close();

  return {
    amountOut: result.amountOut || result.AmountOut,
    stats: result.stats || result.Stats,
    warmMs,
    hotMs,
  };
}

// ── Main ─────────────────────────────────────────────────────────────

async function main() {
  // 1. Native quoter
  const quoter = await createQuoter({ stateServerUrl: STATE_SERVER_URL });

  // Normal mode (EVM + formula)
  const t0 = performance.now();
  const normal = await quoter.findRoute(WAVAX, USDC, ONE_AVAX);
  const normalMs = performance.now() - t0;

  // Formula-only mode (warm-up)
  await quoter.findRoute(WAVAX, USDC, ONE_AVAX, { formulaOnly: true });

  // Formula-only mode (timed)
  const t1 = performance.now();
  const formulaOnly = await quoter.findRoute(WAVAX, USDC, ONE_AVAX, { formulaOnly: true });
  const formulaMs = performance.now() - t1;

  quoter.close();

  // 2. Wasm formula-only
  const wasm = await runWasm();

  // ── Results ────────────────────────────────────────────────────────
  const pad = (s: string, n: number) => s.padEnd(n);
  const fmtUsdc = (v: string | bigint) => (Number(BigInt(v)) / 1e6).toFixed(6);
  const fmtMs = (v: number) => v.toFixed(1) + "ms";

  console.log("\n" + "═".repeat(65));
  console.log("  1 WAVAX → USDC Quote Comparison");
  console.log("═".repeat(65));
  console.log(`  ${pad("Mode", 30)} ${pad("USDC Out", 15)} ${pad("Time", 10)}`);
  console.log("─".repeat(65));
  console.log(`  ${pad("Native (EVM + Formula)", 30)} ${pad(fmtUsdc(normal!.amountOut), 15)} ${fmtMs(normalMs)}`);
  console.log(`  ${pad("Native (Formula-only)", 30)} ${pad(fmtUsdc(formulaOnly!.amountOut), 15)} ${fmtMs(formulaMs)}`);
  console.log(`  ${pad("Wasm (Formula-only, warm)", 30)} ${pad(fmtUsdc(wasm.amountOut), 15)} ${fmtMs(wasm.warmMs)}`);
  console.log(`  ${pad("Wasm (Formula-only, hot)", 30)} ${pad(fmtUsdc(wasm.amountOut), 15)} ${fmtMs(wasm.hotMs)}`);
  console.log("═".repeat(65));

  const speedup = normalMs / formulaMs;
  const wasmSlowdown = wasm.hotMs / formulaMs;
  console.log(`\n  Native formula-only is ${speedup.toFixed(0)}x faster than EVM+Formula`);
  console.log(`  Wasm formula-only is ${wasmSlowdown.toFixed(1)}x slower than native formula-only`);

  process.exit(0);
}

main().catch(err => {
  console.error(err);
  process.exit(1);
});
