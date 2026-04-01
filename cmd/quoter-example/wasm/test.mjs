import { readFile } from "fs/promises";
import { argv } from "process";

const execJs = await readFile(new URL("./wasm_exec.js", import.meta.url), "utf8");
new Function(execJs)();

const go = new Go();

console.log("loading wasm...");
const t0 = Date.now();
const wasmBytes = await readFile(new URL("./quoter.wasm", import.meta.url));
const { instance } = await WebAssembly.instantiate(wasmBytes, go.importObject);
console.log(`instantiated in ${Date.now() - t0}ms (${wasmBytes.length} bytes)`);

go.run(instance);
await new Promise((r) => setTimeout(r, 100));

const WAVAX = "0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7";
const USDC = "0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E";

const wsUrl = argv[2] || "ws://localhost:7449/live";
const poolLimit = parseInt(argv[3] || "2000");

console.log(`connecting to ${wsUrl} with poolLimit=${poolLimit}...`);
const t1 = Date.now();
await globalThis.connect(wsUrl, poolLimit);
console.log(`connected in ${Date.now() - t1}ms`);

console.log("\n=== 1 WAVAX → USDC ===");
const t2 = Date.now();
const result1 = await globalThis.quote(WAVAX, USDC, "1000000000000000000");
console.log(`quoted in ${Date.now() - t2}ms`);
console.log(JSON.stringify(result1, null, 2));

const usdcOut = result1.forward.amountOut;
console.log(`\n=== ${usdcOut} USDC → WAVAX ===`);
const t3 = Date.now();
const result2 = await globalThis.quote(USDC, WAVAX, usdcOut);
console.log(`quoted in ${Date.now() - t3}ms`);
console.log(JSON.stringify(result2, null, 2));

process.exit(0);
