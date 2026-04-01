// Test: poll for new blocks, fire one quote per block.
// Usage: node test_blocks.mjs [wsUrl] [poolLimit] [numBlocks]

import { readFile } from "fs/promises";
import { argv } from "process";

const execJs = await readFile(new URL("./wasm_exec.js", import.meta.url), "utf8");
new Function(execJs)();

const go = new Go();

console.log("loading wasm...");
const wasmBytes = await readFile(new URL("./quoter.wasm", import.meta.url));
const { instance } = await WebAssembly.instantiate(wasmBytes, go.importObject);

go.run(instance);
await new Promise((r) => setTimeout(r, 100));

const WAVAX = "0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7";
const USDC  = "0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E";

const wsUrl     = argv[2] || "ws://localhost:7449/live";
const poolLimit = parseInt(argv[3] || "2000");
const maxBlocks = parseInt(argv[4] || "5");

console.log(`connecting (poolLimit=${poolLimit})...`);
const t0 = Date.now();
await globalThis.connect(wsUrl, poolLimit, 3);
console.log(`connected in ${Date.now() - t0}ms`);

// Warmup quote
console.log("\n=== warmup quote ===");
globalThis.resetFetchCount();
const tw = Date.now();
const warmup = await globalThis.quote(WAVAX, USDC, "1000000000000000000");
console.log(`warmup: ${Date.now() - tw}ms  out=${warmup.forward.amountOut}  fetches=${globalThis.getFetchCount()}`);

// Track latest block
let latestBlock = 0;
globalThis.subscribeBlocks((block, timestamp) => {
  latestBlock = block;
});

// Blocking loop: wait for block change, quote once
let lastBlock = 0;
console.log(`\n=== waiting for ${maxBlocks} blocks ===`);

for (let i = 0; i < maxBlocks; ) {
  // Spin until block changes
  while (latestBlock === lastBlock) {
    await new Promise((r) => setTimeout(r, 1));
  }
  lastBlock = latestBlock;
  i++;

  globalThis.resetFetchCount();
  const t = Date.now();
  const result = await globalThis.quote(WAVAX, USDC, "1000000000000000000");
  const ms = Date.now() - t;
  const fetches = globalThis.getFetchCount();
  const fwd = result.forward;
  console.log(`block ${lastBlock}  quote: ${ms}ms  out=${fwd.amountOut}  fetches=${fetches}`);
}

console.log("\ndone");
process.exit(0);
