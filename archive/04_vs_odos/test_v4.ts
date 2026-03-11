import { spawn } from "node:child_process";
import path from "node:path";
import { loadDotEnv } from "../../utils/env.ts";
loadDotEnv();

const rpcUrl = process.env.RPC_URL || "https://api.avax.network/ext/bc/C/rpc";
const wsRpcUrl = process.env.WS_RPC_URL || rpcUrl.replace("http://", "ws://").replace("https://", "wss://").replace("/rpc", "/ws");
const manifestPath = path.join(import.meta.dirname, "../../pathfinders/simple-bfs/Cargo.toml");
const overridesPath = path.join(import.meta.dirname, "../../pathfinders/simple-bfs/data/token_overrides.json");
const stateCacheDir = path.join(import.meta.dirname, "state_cache");

const block = "80341389";
const proc = spawn("cargo", ["run", "--release", "--manifest-path", manifestPath, "--", "quote", overridesPath, block], {
  env: { ...process.env, WS_RPC_URL: wsRpcUrl, STATE_CACHE_DIR: stateCacheDir },
  stdio: ["pipe", "pipe", "pipe"],
});
proc.stderr!.on("data", (d: Buffer) => process.stderr.write(d));

let buffer = "";
let nextId = 1;
const pending = new Map<number, { resolve: (v: any) => void }>();
const readyP = new Promise<void>((resolve) => {
  proc.stdout!.on("data", (d: Buffer) => {
    buffer += d.toString();
    let nl: number;
    while ((nl = buffer.indexOf("\n")) !== -1) {
      const line = buffer.slice(0, nl); buffer = buffer.slice(nl + 1);
      if (!line.trim()) continue;
      const msg = JSON.parse(line);
      if (msg.op === "ready") { resolve(); continue; }
      const p = pending.get(msg.id);
      if (p) { pending.delete(msg.id); p.resolve(msg); }
    }
  });
});

function send(op: string, params: any): Promise<any> {
  const id = nextId++;
  return new Promise((resolve) => {
    pending.set(id, { resolve });
    proc.stdin!.write(JSON.stringify({ op, id, ...params }) + "\n");
  });
}

await readyP;
console.log("Ready\n");

const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const USDt = "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7";

// Test 1: Only the V3 pool
const v3Pools = `80341389\n0x1150403b19315615aad1638d9dd86cd866b2f456:uniswap_v3:0:80169592:${USDt}:${USDC}\n`;
const r1 = await send("quote_with_pools", { pools: v3Pools, token_in: USDC, token_out: USDt, amount: "10000000", input_avax: "0", max_hops: 4, gas_limit: 2000000 });
console.log(`V3 only:  amount_out=${r1.amount_out}`);

// Test 2: Only the top V4 pool
const v4Pools = `80341389\n0xfe74ff9963652d64086e4467e64ceae7847ebf01:uniswap_v4:9:80169763:${USDt}:${USDC}:@id=0xfe74ff9963652d64086e4467e64ceae7847ebf0139cb4046bcbe02c86e48f256,fee=18,ts=1,hooks=0x0000000000000000000000000000000000000000\n`;
const r2 = await send("quote_with_pools", { pools: v4Pools, token_in: USDC, token_out: USDt, amount: "10000000", input_avax: "0", max_hops: 4, gas_limit: 2000000 });
console.log(`V4 only:  amount_out=${r2.amount_out}  gas=${r2.gas}  evm_calls=${r2.evm_calls}  hops=${r2.hops}`);

// Test 3: Both — which does BFS pick?
const bothPools = `80341389\n0x1150403b19315615aad1638d9dd86cd866b2f456:uniswap_v3:0:80169592:${USDt}:${USDC}\n0xfe74ff9963652d64086e4467e64ceae7847ebf01:uniswap_v4:9:80169763:${USDt}:${USDC}:@id=0xfe74ff9963652d64086e4467e64ceae7847ebf0139cb4046bcbe02c86e48f256,fee=18,ts=1,hooks=0x0000000000000000000000000000000000000000\n`;
const r3 = await send("quote_with_pools", { pools: bothPools, token_in: USDC, token_out: USDt, amount: "10000000", input_avax: "0", max_hops: 4, gas_limit: 2000000 });
console.log(`Both:     amount_out=${r3.amount_out}  picked=${r3.route?.[0]?.pool?.slice(0,10)}`);

proc.stdin!.end();
proc.kill();
process.exit(0);
