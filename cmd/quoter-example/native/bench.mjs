// Benchmark: sequential quotes to measure native performance.
// Usage: node bench.mjs [poolLimit] [wsUrl]

import { spawn } from "child_process";
import { createInterface } from "readline";
import { argv } from "process";

const poolLimit = argv[2] || "2000";
const wsUrl = argv[3] || "ws://localhost:7449/live";

console.log(`spawning native quoter (poolLimit=${poolLimit})...`);

const proc = spawn("go", [
  "run", "./cmd/quoter-example/native/",
  "--pool-limit", poolLimit,
  "--max-hops", "3",
  "--state-server", wsUrl,
], { stdio: ["pipe", "pipe", "inherit"] });

const rl = createInterface({ input: proc.stdout });
const pending = new Map();
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

// Wait for "ready" on stderr (comes through inherit, just give it time)
await new Promise((r) => setTimeout(r, 500));

// Warmup
console.log("\n=== warmup ===");
const tw = Date.now();
const w = await quote(WAVAX, USDC, "1000000000000000000");
console.log(`warmup: ${Date.now() - tw}ms  out=${w.forward.amountOut}`);

// 5 sequential quotes
console.log("\n=== 5 sequential quotes ===");
for (let i = 0; i < 5; i++) {
  const t = Date.now();
  const r = await quote(WAVAX, USDC, "1000000000000000000");
  console.log(`quote ${i+1}: ${Date.now() - t}ms  out=${r.forward.amountOut}`);
}

proc.kill();
process.exit(0);
