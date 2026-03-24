// sdk-browser.ts — Browser EVM quoter SDK
//
// WASM-based EVM quoter for browser environments.
// Requires wasm_exec.js loaded via <script> tag (sets globalThis.Go).
// Connects to state-server via native browser WebSocket.
//
// Usage:
//   const quoter = await createBrowserQuoter({
//     stateServerUrl: "ws://localhost:7449",
//     wasmUrl: "/evm-quoter/bin/harness.wasm",
//   });
//   const results = await quoter.quotePoolBatch(pools, stateOverrides);
//   quoter.close();

import { encodeFunctionData } from "viem";

// ── Constants ────────────────────────────────────────────────────────

export const ROUTER = "0x000000000000000000000000cafebabe00facade";
export const DUMMY_SENDER = "0x000000000000000000000000000000000000dEaD";

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
}] as const;

export function encodeSwapSingle(pool: string, poolType: number, tokenIn: string, tokenOut: string, amountIn: bigint) {
  return encodeFunctionData({
    abi: swapAbi, functionName: "executeSwap",
    args: [[pool as `0x${string}`], [poolType], [tokenIn as `0x${string}`, tokenOut as `0x${string}`], [amountIn], ["0x"]],
  });
}

// ── State server WebSocket helpers ───────────────────────────────────

let ssReqId = 0;

function stateServerRequest(ws: WebSocket, method: string, params: any): Promise<string> {
  return new Promise((resolve, reject) => {
    const id = ++ssReqId;
    const handler = (event: MessageEvent) => {
      let msg: any;
      try { msg = JSON.parse(event.data); } catch { return; }
      if (msg.id !== id) return;
      ws.removeEventListener("message", handler);
      if (msg.error) reject(new Error(msg.error.message || JSON.stringify(msg.error)));
      else resolve(msg.result.value);
    };
    ws.addEventListener("message", handler);
    ws.send(JSON.stringify({ jsonrpc: "2.0", id, method, params }));
  });
}

// ── WASM backend (browser) ──────────────────────────────────────────

declare const Go: any;

interface BrowserQuoterOpts {
  stateServerUrl?: string;
  wasmUrl: string;
  onStatus?: (msg: string) => void;
}

async function createWasmBackend(opts: BrowserQuoterOpts) {
  const log = opts.onStatus || ((msg: string) => console.log(`[wasm] ${msg}`));

  // Ensure Go runtime is loaded (via <script src="wasm_exec.js">)
  const GoClass = (globalThis as any).Go;
  if (!GoClass) throw new Error("Go WASM runtime not loaded. Add <script src=\"wasm_exec.js\"></script> to your HTML.");

  // Connect to state server
  let stateWs: WebSocket | null = null;
  let blockNumber = 80_000_000;
  let timestamp = 0;
  let closed = false;
  let initialDumpPromise: Promise<any> | null = null;

  if (opts.stateServerUrl) {
    log("Connecting to state server...");
    stateWs = await new Promise<WebSocket>((resolve, reject) => {
      const ws = new WebSocket(opts.stateServerUrl!);
      ws.onerror = () => reject(new Error("WebSocket connection failed"));
      ws.onopen = () => resolve(ws);
    });

    initialDumpPromise = new Promise((resolve) => {
      const handler = (event: MessageEvent) => {
        const msg = JSON.parse(event.data);
        if (msg.type === "initial_dump") {
          stateWs!.removeEventListener("message", handler);
          resolve(msg);
        }
      };
      stateWs!.addEventListener("message", handler);
    });
  }

  // Register async fetch callbacks for Go→JS state bridging
  const noServer = () => Promise.resolve("0x" + "0".repeat(64));
  const g = globalThis as any;

  g.__goFetchStorageAsync = stateWs
    ? (addr: string, slot: string) => stateServerRequest(stateWs!, "state_getStorageAt", { address: addr, slot, blockNumber })
    : noServer;
  g.__goFetchBalanceAsync = stateWs
    ? (addr: string) => stateServerRequest(stateWs!, "state_getBalance", { address: addr, blockNumber })
    : () => Promise.resolve("0x0");
  g.__goFetchNonceAsync = stateWs
    ? (addr: string) => stateServerRequest(stateWs!, "state_getNonce", { address: addr, blockNumber })
    : () => Promise.resolve("0x0");
  g.__goFetchCodeAsync = stateWs
    ? (addr: string) => stateServerRequest(stateWs!, "state_getCode", { address: addr, blockNumber })
    : () => Promise.resolve("0x");
  g.__goFetchBlockHashAsync = () => Promise.resolve("0x" + "0".repeat(64));

  // Load and run WASM
  log("Loading WASM binary...");
  const go = new GoClass();
  const wasmResp = await fetch(opts.wasmUrl);
  const { instance } = await WebAssembly.instantiateStreaming(wasmResp, go.importObject);
  go.run(instance);

  // Wait for Go to register global functions
  let tries = 0;
  while (!g.__goReady) {
    if (++tries > 200) throw new Error("WASM: __goReady not set after 2s");
    await new Promise((r) => setTimeout(r, 10));
  }
  log("WASM ready");

  // Process initial_dump and set up block_diff listener
  if (stateWs && initialDumpPromise) {
    log("Processing initial state dump...");
    const msg = await initialDumpPromise;
    blockNumber = msg.blockNumber;
    timestamp = msg.timestamp || 0;
    g.__goSetBlock(blockNumber, timestamp, msg.baseFee || 0, msg.gasLimit || 0);

    let storageCount = 0;
    const acctData = new Map<string, any>();

    for (const [key, value] of msg.entries) {
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

    log(`State loaded: block=${blockNumber}, ${storageCount} storage, ${acctData.size} accounts`);

    stateWs.addEventListener("message", (event: MessageEvent) => {
      if (closed) return;
      let msg: any;
      try { msg = JSON.parse(event.data); } catch { return; }
      if (msg.type === "block_diff") {
        blockNumber = msg.blockNumber;
        timestamp = msg.timestamp || timestamp;
        if (typeof g.__goSetBlock === "function") {
          g.__goSetBlock(blockNumber, timestamp, msg.baseFee || 0, msg.gasLimit || 0);
        }
        for (const [key, value] of msg.entries) {
          if (key.startsWith("s:")) {
            const parts = key.split(":");
            if (parts.length === 3 && typeof g.__goPrefillStorage === "function") {
              g.__goPrefillStorage(parts[1], parts[2], value);
            }
          }
        }
      }
    });
  }

  return {
    ethCall({ to, data, from, stateOverrides }: { to: string; data: string; from?: string; stateOverrides?: any }) {
      const overridesJson = stateOverrides ? JSON.stringify(stateOverrides) : null;
      const fromAddr = from || "0x0000000000000000000000000000000000000000";
      return new Promise<any>((resolve, reject) => {
        g.__goEthCall(fromAddr, to, data, (resultJson: string) => {
          try { resolve(JSON.parse(resultJson)); }
          catch (e) { reject(e); }
        }, overridesJson);
      });
    },
    async ethCallBatch(calls: any[], { stateOverrides }: { stateOverrides?: any } = {}) {
      const overridesJson = stateOverrides ? JSON.stringify(stateOverrides) : "";
      const callsJson = JSON.stringify(calls);
      return new Promise<any>((resolve, reject) => {
        g.__goEthCallBatch(callsJson, overridesJson, (resultJson: string) => {
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
      ]) delete (globalThis as any)[name];
    },
  };
}

// ── Public API ───────────────────────────────────────────────────────

export async function createBrowserQuoter(opts: BrowserQuoterOpts) {
  const backend = await createWasmBackend(opts);

  return {
    async quotePool(pool: string, poolType: number, tokenIn: string, tokenOut: string, amountIn: bigint, stateOverrides?: any) {
      const calldata = encodeSwapSingle(pool, poolType, tokenIn, tokenOut, amountIn);
      try {
        const result = await backend.ethCall({ to: ROUTER, data: calldata, from: DUMMY_SENDER, stateOverrides });
        if (result.error) return { ok: false as const, error: result.error, returnData: result.returnData };
        return { ok: true as const, returnData: result.returnData, gasUsed: result.gasUsed };
      } catch (e: any) {
        return { ok: false as const, error: e.message };
      }
    },

    async quotePoolBatch(pools: { pool: string; poolType: number; tokenIn: string; tokenOut: string; amountIn: bigint }[], stateOverrides?: any) {
      const calls = pools.map(p => ({
        to: ROUTER,
        data: encodeSwapSingle(p.pool, p.poolType, p.tokenIn, p.tokenOut, p.amountIn),
        from: DUMMY_SENDER,
      }));
      const batch = await backend.ethCallBatch(calls, { stateOverrides });
      const results: any = batch.results.map((r: any) => {
        if (r.error) return { ok: false, error: r.error, returnData: r.returnData, gasUsed: r.gasUsed };
        return { ok: true, returnData: r.returnData, gasUsed: r.gasUsed };
      });
      results.cacheMisses = batch.cacheMisses || 0;
      return results;
    },

    ethCall: backend.ethCall.bind(backend),
    ethCallBatch: backend.ethCallBatch.bind(backend),
    close: backend.close.bind(backend),
  };
}
