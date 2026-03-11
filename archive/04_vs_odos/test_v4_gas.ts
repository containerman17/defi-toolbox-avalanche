import { spawn } from "node:child_process";
import path from "node:path";
import { loadDotEnv } from "../../utils/env.ts";
loadDotEnv();

const wsRpcUrl = (process.env.WS_RPC_URL || process.env.RPC_URL || "ws://127.0.0.1:9650/ext/bc/C/ws").replace("http://", "ws://").replace("https://", "wss://").replace("/rpc", "/ws");
const manifestPath = path.join(import.meta.dirname, "../../pathfinders/simple-bfs/Cargo.toml");
const overridesPath = path.join(import.meta.dirname, "../../pathfinders/simple-bfs/data/token_overrides.json");
const stateCacheDir = path.join(import.meta.dirname, "state_cache");

const proc = spawn("cargo", ["run", "--release", "--manifest-path", manifestPath, "--", "quote", overridesPath, "80341389"], {
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

const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const USDt = "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7";
const v4Pools = `80341389\n0xfe74ff9963652d64086e4467e64ceae7847ebf01:uniswap_v4:9:80169763:${USDt}:${USDC}:@id=0xfe74ff9963652d64086e4467e64ceae7847ebf0139cb4046bcbe02c86e48f256,fee=18,ts=1,hooks=0x0000000000000000000000000000000000000000\n`;

// Test with different gas limits
for (const gas of [2_000_000, 5_000_000, 10_000_000, 40_000_000]) {
  const r = await send("quote_with_pools", { pools: v4Pools, token_in: USDC, token_out: USDt, amount: "10000000", input_avax: "0", max_hops: 1, gas_limit: gas });
  console.log(`gas=${gas.toLocaleString().padStart(12)}: amount_out=${r.amount_out}`);
}

proc.stdin!.end();
proc.kill();
process.exit(0);
