// sdk.ts — EVM quoter SDK
// Creates a quoter that runs EVM locally (native Go binary or WASM),
// connected to a state server for blockchain state.
//
// Usage:
//   const quoter = await createQuoter("native", { stateServerUrl: "ws://..." })
//   const quoter = await createQuoter("wasm", { stateServerUrl: "ws://..." })
//   const results = await quoter.quotePoolBatch(pools)
//   quoter.close()

import { spawn } from "node:child_process";
import { createRequire } from "node:module";
import { readFile } from "node:fs/promises";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { WebSocket } from "ws";
import { encodeFunctionData } from "viem";
import { buildStateOverrides as _buildStateOverrides } from "../router/overrides.ts";

const __dirname = dirname(fileURLToPath(import.meta.url));
const BIN = join(__dirname, "bin");
const REPO = join(__dirname, "..");

// ── Constants ─────────────────────────────────────────────────────────

export const ROUTER = "0x000000000000000000000000cafebabe00facade";
export const DUMMY_SENDER = "0x000000000000000000000000000000000000dEaD";
const POOLS_PATH = join(REPO, "pool-collector/data/pools.txt");

export const STARTER_TOKENS = {
  "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e": { symbol: "USDC", decimals: 6, amount: 1_000_000n },
  "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7": { symbol: "USDT", decimals: 6, amount: 1_000_000n },
  "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7": { symbol: "WAVAX", decimals: 18, amount: 1_000_000_000_000_000_000n },
};

export const POOL_TYPE_MAP = {
  uniswap_v3: 0, pharaoh_v3: 0, algebra: 1, lfj_v1: 2, lfj_v2: 3,
  dodo: 4, woofi: 5, woofi_v2: 5, balancer_v3: 6, pharaoh_v1: 7,
  pangolin_v2: 8, arena_v2: 8, uniswap_v2: 8, sushiswap_v2: 8,
  elkdex: 8, lydia: 8, vapordex: 8, swapsicle: 8,
  canary: 8, yetiswap: 8, oliveswap: 8, partyswap: 8,
  complus: 8, hurricane: 8, radioshack: 8, thorus: 8,
  hakuswap: 8, zeroex: 8, fraxswap: 8,
  uniswap_v4: 9,
};

// ── Swap ABI encoding ────────────────────────────────────────────────

const swapAbi = [{
  name: "executeSwap", type: "function",
  inputs: [
    { name: "pools", type: "address[]" },
    { name: "poolTypes", type: "uint8[]" },
    { name: "tokens", type: "address[]" },
    { name: "amountsIn", type: "uint256[]" },
    { name: "extraDatas", type: "bytes[]" },
  ],
  outputs: [{ type: "uint256" }],
}];

export function encodeSwapSingle(pool, poolType, tokenIn, tokenOut, amountIn) {
  return encodeFunctionData({
    abi: swapAbi, functionName: "executeSwap",
    args: [[pool], [poolType], [tokenIn, tokenOut], [amountIn], ["0x"]],
  });
}

// ── Pool loading ─────────────────────────────────────────────────────

export function loadPools(path, limit) {
  const filePath = path || POOLS_PATH;
  const lines = readFileSync(filePath, "utf-8").split("\n").filter(l => l.includes(":"));
  const pools = [];
  for (const line of lines) {
    const parts = line.split(":");
    if (parts.length < 6) continue;
    const [poolAddr, typeName, _typeId, _block, token0, token1raw] = parts;
    const token1 = token1raw.split(":")[0];

    const starterKey = Object.keys(STARTER_TOKENS).find(
      t => t === token0.toLowerCase() || t === token1.toLowerCase()
    );
    if (!starterKey) continue;

    const poolType = POOL_TYPE_MAP[typeName];
    if (poolType === undefined) continue;
    if (poolType === 9) continue; // Skip UniV4

    const tokenIn = starterKey;
    const tokenOut = token0.toLowerCase() === starterKey ? token1.toLowerCase() : token0.toLowerCase();
    const starter = STARTER_TOKENS[starterKey];

    pools.push({ pool: poolAddr, poolType, typeName, tokenIn, tokenOut, amountIn: starter.amount });
    if (limit && pools.length >= limit) break;
  }
  return pools;
}

// ── State overrides ──────────────────────────────────────────────────

export function buildStateOverrides(pools, opts: { tokenAmounts?: Map<string, bigint> } = {}) {
  const tokenAmounts = opts.tokenAmounts || new Map();

  // Default: set balance for each unique input token from starter amounts
  if (tokenAmounts.size === 0) {
    const inputTokens = [...new Set(pools.map(p => p.tokenIn))];
    for (const token of inputTokens) {
      const starter = STARTER_TOKENS[token];
      if (starter) tokenAmounts.set(token, starter.amount);
    }
  }

  const overrides = _buildStateOverrides({
    routerAddress: ROUTER,
    tokenAmounts,
  });

  return { overrides, overrideCount: tokenAmounts.size };
}

// ── State server WebSocket helpers ───────────────────────────────────

let ssReqId = 0;

function stateServerRequest(ws, method, params) {
  return new Promise((resolve, reject) => {
    const id = ++ssReqId;
    const handler = (data) => {
      let msg;
      try { msg = JSON.parse(data.toString()); } catch { return; }
      if (msg.id !== id) return;
      ws.off("message", handler);
      if (msg.error) reject(new Error(msg.error.message || JSON.stringify(msg.error)));
      else resolve(msg.result.value);
    };
    ws.on("message", handler);
    ws.send(JSON.stringify({ jsonrpc: "2.0", id, method, params }));
  });
}

// ── Native backend ──────────────────────────────────────────────────

function createNativeBackend(stateServerUrl) {
  const args = stateServerUrl ? ["--state-server", stateServerUrl] : [];
  // Pass formula registry path
  const registryPath = join(__dirname, "go/formulas/registry.txt");
  args.push("--registry", registryPath);

  const child = spawn(join(BIN, "harness-native"), args, {
    stdio: ["pipe", "pipe", "inherit"],
  });

  let nextId = 1;
  const pending = new Map();
  let buffer = "";

  child.stdout.on("data", (chunk) => {
    buffer += chunk.toString();
    let nl;
    while ((nl = buffer.indexOf("\n")) !== -1) {
      const line = buffer.slice(0, nl).trim();
      buffer = buffer.slice(nl + 1);
      if (!line) continue;
      try {
        const msg = JSON.parse(line);
        const p = pending.get(msg.id);
        if (p) {
          pending.delete(msg.id);
          if (msg.error) p.reject(new Error(msg.error));
          else p.resolve(msg.result);
        }
      } catch {}
    }
  });

  const readyPromise = stateServerUrl
    ? new Promise((r) => setTimeout(r, 2000))
    : Promise.resolve();

  const cleanup = () => { if (!child.killed) child.kill(); };
  process.on("exit", cleanup);
  process.on("SIGINT", cleanup);
  process.on("SIGTERM", cleanup);

  return {
    readyPromise,
    ethCall({ to, data, from, stateOverrides }) {
      const id = nextId++;
      return new Promise((resolve, reject) => {
        pending.set(id, { resolve, reject });
        const params = {
          to: to || "0x0000000000000000000000000000000000000000",
          data: data || "0x",
          from: from || "0x0000000000000000000000000000000000000000",
        };
        if (stateOverrides) params.stateOverrides = stateOverrides;
        child.stdin.write(JSON.stringify({ id, method: "eth_call", params }) + "\n");
      });
    },
    async ethCallBatch(calls, { stateOverrides } = {}) {
      const id = nextId++;
      const raw = await new Promise((resolve, reject) => {
        pending.set(id, { resolve, reject });
        child.stdin.write(JSON.stringify({
          id, method: "eth_call_batch",
          params: { stateOverrides, calls },
        }) + "\n");
      });
      return { results: Array.isArray(raw) ? raw : raw.results, cacheMisses: raw.cacheMisses || 0 };
    },
    close() {
      process.removeListener("exit", cleanup);
      process.removeListener("SIGINT", cleanup);
      process.removeListener("SIGTERM", cleanup);
      child.stdin.end();
      child.kill();
    },
  };
}

// ── WASM backend ────────────────────────────────────────────────────

async function createWasmBackend(stateServerUrl) {
  // Load wasm_exec.js
  const execPath = join(BIN, "wasm_exec.js");
  const execSrc = await readFile(execPath, "utf-8");
  const script = new Function("require", "process", execSrc);
  const require = createRequire(import.meta.url);
  script(require, process);

  // Connect to state server
  let stateWs = null;
  let blockNumber = 80_000_000;
  let timestamp = 0;
  let closed = false;
  let initialDumpPromise = null;

  if (stateServerUrl) {
    stateWs = await new Promise((resolve, reject) => {
      const ws = new WebSocket(stateServerUrl, { maxPayload: 512 * 1024 * 1024 });
      ws.on("error", reject);
      ws.on("open", () => resolve(ws));
    });

    initialDumpPromise = new Promise((resolve) => {
      const handler = (data) => {
        const msg = JSON.parse(data.toString());
        if (msg.type === "initial_dump") {
          stateWs.off("message", handler);
          resolve(msg);
        }
      };
      stateWs.on("message", handler);
    });
  }

  // Register async fetch callbacks for Go→JS state bridging
  const noServer = () => Promise.resolve("0x" + "0".repeat(64));

  globalThis.__goFetchStorageAsync = stateWs
    ? (addr, slot) => stateServerRequest(stateWs, "state_getStorageAt", { address: addr, slot, blockNumber })
    : noServer;
  globalThis.__goFetchBalanceAsync = stateWs
    ? (addr) => stateServerRequest(stateWs, "state_getBalance", { address: addr, blockNumber })
    : () => Promise.resolve("0x0");
  globalThis.__goFetchNonceAsync = stateWs
    ? (addr) => stateServerRequest(stateWs, "state_getNonce", { address: addr, blockNumber })
    : () => Promise.resolve("0x0");
  globalThis.__goFetchCodeAsync = stateWs
    ? (addr) => stateServerRequest(stateWs, "state_getCode", { address: addr, blockNumber })
    : () => Promise.resolve("0x");
  globalThis.__goFetchBlockHashAsync = () => Promise.resolve("0x" + "0".repeat(64));

  // Load and run WASM
  const GoClass = globalThis.Go;
  const go = new GoClass();
  const wasmBuf = await readFile(join(BIN, "harness.wasm"));
  const { instance } = await WebAssembly.instantiate(wasmBuf, go.importObject);
  const running = go.run(instance);

  // Wait for Go to register global functions
  let tries = 0;
  while (!globalThis.__goReady) {
    if (++tries > 200) throw new Error("WASM: __goReady not set");
    await new Promise((r) => setTimeout(r, 10));
  }

  // Process initial_dump and set up block_diff listener
  if (stateWs && initialDumpPromise) {
    const msg = await initialDumpPromise;
    blockNumber = msg.blockNumber;
    timestamp = msg.timestamp || 0;
    globalThis.__goSetBlock(blockNumber, timestamp, msg.baseFee || 0, msg.gasLimit || 0);

    let storageCount = 0;
    const acctData = new Map();

    for (const [key, value] of msg.entries) {
      if (key.startsWith("s:")) {
        const parts = key.split(":");
        if (parts.length === 3) {
          globalThis.__goPrefillStorage(parts[1], parts[2], value);
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
      globalThis.__goPrefillAccount(addr, d.balance || "0x0", d.nonce ? parseInt(d.nonce, 16) : 0, d.code || "0x");
    }

    console.error(`[wasm] initial_dump: block=${blockNumber}, ${storageCount} storage, ${acctData.size} accounts`);

    stateWs.on("message", (data) => {
      if (closed) return;
      let msg;
      try { msg = JSON.parse(data.toString()); } catch { return; }
      if (msg.type === "block_diff") {
        blockNumber = msg.blockNumber;
        timestamp = msg.timestamp || timestamp;
        if (typeof globalThis.__goSetBlock === "function") {
          globalThis.__goSetBlock(blockNumber, timestamp, msg.baseFee || 0, msg.gasLimit || 0);
        }
        for (const [key, value] of msg.entries) {
          if (key.startsWith("s:")) {
            const parts = key.split(":");
            if (parts.length === 3 && typeof globalThis.__goPrefillStorage === "function") {
              globalThis.__goPrefillStorage(parts[1], parts[2], value);
            }
          }
        }
      }
    });
  }

  return {
    ethCall({ to, data, from, stateOverrides }) {
      const overridesJson = stateOverrides ? JSON.stringify(stateOverrides) : null;
      const fromAddr = from || "0x0000000000000000000000000000000000000000";
      return new Promise((resolve, reject) => {
        globalThis.__goEthCall(fromAddr, to, data, (resultJson) => {
          try { resolve(JSON.parse(resultJson)); }
          catch (e) { reject(e); }
        }, overridesJson);
      });
    },
    async ethCallBatch(calls, { stateOverrides } = {}) {
      const overridesJson = stateOverrides ? JSON.stringify(stateOverrides) : "";
      const callsJson = JSON.stringify(calls);
      return new Promise((resolve, reject) => {
        globalThis.__goEthCallBatch(callsJson, overridesJson, (resultJson) => {
          try { resolve(JSON.parse(resultJson)); }
          catch (e) { reject(e); }
        });
      });
    },
    close() {
      closed = true;
      if (stateWs) stateWs.close();
      for (const name of [
        "increment", "__goReady", "__goEthCall", "__goEthCallBatch",
        "__goSetBlock", "__goPrefillStorage", "__goPrefillAccount",
        "__goFetchStorageAsync", "__goFetchBalanceAsync", "__goFetchNonceAsync",
        "__goFetchCodeAsync", "__goFetchBlockHashAsync",
      ]) delete globalThis[name];
    },
  };
}

// ── Public API ───────────────────────────────────────────────────────

export async function createQuoter(mode, opts = {}) {
  const stateServerUrl = opts.stateServerUrl || null;

  let backend;
  if (mode === "native") {
    backend = createNativeBackend(stateServerUrl);
    await backend.readyPromise;
  } else if (mode === "wasm") {
    backend = await createWasmBackend(stateServerUrl);
  } else {
    throw new Error(`Unknown mode: ${mode}. Use "native" or "wasm".`);
  }

  return {
    /** Quote a single pool */
    async quotePool(pool, poolType, tokenIn, tokenOut, amountIn, stateOverrides) {
      const calldata = encodeSwapSingle(pool, poolType, tokenIn, tokenOut, amountIn);
      try {
        const result = await backend.ethCall({
          to: ROUTER, data: calldata, from: DUMMY_SENDER, stateOverrides,
        });
        if (result.error) return { ok: false, error: result.error, returnData: result.returnData };
        return { ok: true, returnData: result.returnData, gasUsed: result.gasUsed };
      } catch (e) {
        return { ok: false, error: e.message };
      }
    },

    /** Quote a batch of pools. Each pool object: { pool, poolType, tokenIn, tokenOut, amountIn } */
    async quotePoolBatch(pools, stateOverrides) {
      const calls = pools.map(p => ({
        to: ROUTER,
        data: encodeSwapSingle(p.pool, p.poolType, p.tokenIn, p.tokenOut, p.amountIn),
        from: DUMMY_SENDER,
      }));
      const batch = await backend.ethCallBatch(calls, { stateOverrides });
      const results = batch.results.map(r => {
        if (r.error) return { ok: false, error: r.error, returnData: r.returnData, gasUsed: r.gasUsed };
        return { ok: true, returnData: r.returnData, gasUsed: r.gasUsed };
      });
      results.cacheMisses = batch.cacheMisses || 0;
      return results;
    },

    /** Raw ethCall (for advanced use) */
    ethCall: backend.ethCall.bind(backend),

    /** Raw ethCallBatch (for advanced use) */
    ethCallBatch: backend.ethCallBatch.bind(backend),

    close() {
      backend.close();
    },
  };
}
