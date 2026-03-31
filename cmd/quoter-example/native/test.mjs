// Node.js talking to the native quoter binary via stdin/stdout.
// Supports parallel quotes using request IDs.

import { spawn } from "child_process";
import { createInterface } from "readline";
import { argv } from "process";

const poolLimit = argv[2] || "2000";
const wsUrl = argv[3] || "ws://localhost:7449/live";

console.log(`spawning native quoter (poolLimit=${poolLimit})...`);

const proc = spawn("go", [
  "run", "./cmd/quoter-example/native/",
  "--pool-limit", poolLimit,
  "--state-server", wsUrl,
], { stdio: ["pipe", "pipe", "inherit"] });

const rl = createInterface({ input: proc.stdout });
const pending = new Map(); // id -> { resolve, reject }
let nextId = 1;

rl.on("line", (line) => {
  try {
    const resp = JSON.parse(line);
    const id = resp.id;
    const p = pending.get(id);
    if (p) {
      pending.delete(id);
      p.resolve(resp);
    }
  } catch {}
});

function quote(tokenIn, tokenOut, amountIn) {
  const id = nextId++;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    proc.stdin.write(JSON.stringify({ id, tokenIn, tokenOut, amountIn }) + "\n");
  });
}

const WAVAX = "0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7";
const USDC  = "0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E";
const USDt  = "0x9702230A8Ea53601f5cD2dc00fDBc13d4dF4A8c7";
const WETHe = "0x49D5c2BdFfac6CE2BFdB6640F4F80f226bc10bAB";

// --- Sequential warmup ---
console.log("\n=== Sequential: 1 WAVAX → USDC ===");
const t0 = Date.now();
const r0 = await quote(WAVAX, USDC, "1000000000000000000");
console.log(`  ${Date.now() - t0}ms  amountOut=${r0.forward.amountOut}`);

// --- Parallel burst ---
console.log("\n=== Parallel: 4 quotes at once ===");
const t1 = Date.now();
const results = await Promise.all([
  quote(WAVAX, USDC,  "1000000000000000000"),
  quote(WAVAX, USDt,  "1000000000000000000"),
  quote(WAVAX, WETHe, "1000000000000000000"),
  quote(USDC,  WAVAX, "10000000"),
]);
const elapsed = Date.now() - t1;

for (const r of results) {
  const fwd = r.forward;
  console.log(`  id=${r.id}  ${fwd.tokenIn.slice(0,8)}→${fwd.tokenOut.slice(0,8)}  out=${fwd.amountOut}  hops=${fwd.path?.length ?? 0}`);
}
console.log(`  all 4 in ${elapsed}ms`);

// --- Compare: sequential same 4 ---
console.log("\n=== Sequential: same 4 quotes ===");
const t2 = Date.now();
await quote(WAVAX, USDC,  "1000000000000000000");
await quote(WAVAX, USDt,  "1000000000000000000");
await quote(WAVAX, WETHe, "1000000000000000000");
await quote(USDC,  WAVAX, "10000000");
console.log(`  all 4 in ${Date.now() - t2}ms`);

proc.kill();
process.exit(0);
