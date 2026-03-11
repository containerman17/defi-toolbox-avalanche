// BFS routing benchmark — spawns the Rust quoter and runs WAVAX→USDC with first 1000 pools.
import { spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import path from "node:path";

const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const ONE_AVAX = "1000000000000000000";
const MAX_HOPS = 4;
const POOL_LIMIT = parseInt(process.env.POOLS || "1000");
const WARM_RUNS = parseInt(process.env.RUNS || "5");

// Load pools — take first N
const poolsPath = path.join(import.meta.dirname, "../../pools/data/pools.txt");
const allPools = readFileSync(poolsPath, "utf-8");
const lines = allPools.split("\n");
const header = lines[0]; // block number
const poolLines = lines.slice(1).filter(l => l.trim()).slice(0, POOL_LIMIT);
const poolsStr = header + "\n" + poolLines.join("\n");
console.log(`Loaded ${poolLines.length} pools from ${poolsPath}`);

// Spawn Rust quoter
const manifestPath = path.join(import.meta.dirname, "../../pathfinders/simple-bfs/Cargo.toml");
const overridesPath = path.join(import.meta.dirname, "../../pathfinders/simple-bfs/data/token_overrides.json");

const proc = spawn("cargo", ["run", "--release", "--manifest-path", manifestPath, "--", "quote", overridesPath], {
  stdio: ["pipe", "pipe", "pipe"],
});
proc.stderr!.on("data", (d: Buffer) => process.stderr.write(d));

// ndjson protocol
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
console.log(`Ready at block ${ready.block}\n`);

// Cold quote
console.log(`BFS: WAVAX→USDC, 1 AVAX, ${MAX_HOPS} hops, ${poolLines.length} pools`);
const t0 = performance.now();
const cold = await send("quote_with_pools", {
  pools: poolsStr,
  token_in: WAVAX,
  token_out: USDC,
  amount: ONE_AVAX,
  input_avax: ONE_AVAX,
  max_hops: MAX_HOPS,
  gas_limit: 2000000,
});
const coldMs = performance.now() - t0;
console.log(`COLD: ${coldMs.toFixed(0)}ms (server=${cold.elapsed_ms}ms), amount_out=${cold.amount_out}, evm=${cold.evm_calls}, requote=${cold.requote_calls}, hops=${cold.hops}`);
for (const step of cold.route || []) {
  console.log(`  ${step.token_in.slice(0, 10)}→${step.token_out.slice(0, 10)} via ${step.pool.slice(0, 10)} (type=${step.pool_type})`);
}

// Warm quotes
const warmTimes: number[] = [];
for (let i = 0; i < WARM_RUNS; i++) {
  const t = performance.now();
  const q = await send("quote_with_pools", {
    pools: poolsStr,
    token_in: WAVAX,
    token_out: USDC,
    amount: ONE_AVAX,
    input_avax: ONE_AVAX,
    max_hops: MAX_HOPS,
    gas_limit: 2000000,
  });
  const ms = performance.now() - t;
  warmTimes.push(ms);
  console.log(`WARM #${i + 1}: ${ms.toFixed(0)}ms (server=${q.elapsed_ms}ms), amount_out=${q.amount_out}, evm=${q.evm_calls}, requote=${q.requote_calls}`);
}

const avg = warmTimes.reduce((a, b) => a + b, 0) / warmTimes.length;
console.log(`\nAverage warm: ${avg.toFixed(0)}ms (${WARM_RUNS} runs, ${poolLines.length} pools)`);

proc.stdin!.end();
proc.kill();
process.exit(0);
