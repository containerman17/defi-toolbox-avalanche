// Debug: check if extractSplitSteps sees the multiswap pool
import * as fs from "node:fs";
import * as path from "node:path";
import { createPublicClient, http, type Log } from "viem";
import { avalanche } from "viem/chains";
import { loadPools, ERC4626_VAULTS, generateBufferedEdges, type StoredPool } from "hayabusa-pools";

const TRANSFER_TOPIC = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef";
const ZERO_ADDR = "0x0000000000000000000000000000000000000000";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const WOOPP_ADDRESS = "0x5520385bfcf07ec87c4c53a7d8d65595dff69fa4";
const WOOPP_V2_ADDRESS = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const V4_POOL_MANAGER = "0x06380c0e0912312b5150364b9dc4542ba0dbbc85";
const MULTISWAP_POOL = "0x5f1e8ed8468232bab71eda9f4598bda3161f48ea";
const HASHFLOW_VAULTS = new Set(["0x6047b384d58dc7f8f6fef85d75754e6928f06484"]);

function isPoolAddr(addr: string, poolMap: Map<string, StoredPool>): boolean {
  return poolMap.has(addr) || addr === V4_POOL_MANAGER || addr === WOOPP_ADDRESS || addr === WOOPP_V2_ADDRESS || HASHFLOW_VAULTS.has(addr) || addr === MULTISWAP_POOL;
}

interface TransferEvent {
  token: string;
  from: string;
  to: string;
  amount: bigint;
  logIndex: number;
}

function collectTraceLogs(call: any): any[] {
  const logs: any[] = [];
  if (call.logs) {
    for (const log of call.logs) {
      logs.push({ address: (log.address as string).toLowerCase(), topics: log.topics, data: log.data });
    }
  }
  if (call.calls) {
    for (const sub of call.calls) logs.push(...collectTraceLogs(sub));
  }
  return logs;
}

async function main() {
  const poolsPath = path.resolve(import.meta.dirname!, "../pools/data/pools.txt");
  const { pools: rawPoolMap } = loadPools(poolsPath);
  const rawPools = [...rawPoolMap.values()];
  const allPools = [...rawPools, ...ERC4626_VAULTS, ...generateBufferedEdges(rawPools)];
  const poolMap = new Map<string, StoredPool>();
  for (const p of allPools) poolMap.set(p.address.toLowerCase(), p);

  console.log("Pool map has multiswap:", poolMap.has(MULTISWAP_POOL));
  console.log("isPoolAddr(MULTISWAP):", isPoolAddr(MULTISWAP_POOL, poolMap));

  const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });
  const txHash = "0x47e00934c4c69c5f47f3b03b5f7ce3070c983205bab36e63b751504dd488a83d" as `0x${string}`;
  const block = 80127543;
  const tx = await client.getTransaction({ hash: txHash });

  const resp = await fetch("http://localhost:9650/ext/bc/C/rpc", {
    method: "POST",
    headers: {"Content-Type": "application/json"},
    body: JSON.stringify({
      jsonrpc: "2.0",
      method: "debug_traceCall",
      params: [{
        from: tx.from, to: tx.to, data: tx.input,
        value: "0x" + tx.value.toString(16),
        gas: "0x" + (tx.gas * 2n).toString(16),
      }, "0x" + (BigInt(block) - 1n).toString(16), {
        tracer: "callTracer", tracerConfig: { withLog: true }
      }],
      id: 1
    })
  });
  const result: any = await resp.json();
  const allLogs = collectTraceLogs(result.result);

  const transfers: TransferEvent[] = [];
  for (let i = 0; i < allLogs.length; i++) {
    const log = allLogs[i];
    if (log.topics[0] === TRANSFER_TOPIC && log.topics.length >= 3) {
      transfers.push({
        token: log.address,
        from: ("0x" + log.topics[1].slice(26)).toLowerCase(),
        to: ("0x" + log.topics[2].slice(26)).toLowerCase(),
        amount: BigInt(log.data),
        logIndex: i,
      });
    }
  }

  // Simulate extractSplitSteps for MULTISWAP_POOL
  const addrs = new Set<string>();
  for (const t of transfers) { addrs.add(t.from); addrs.add(t.to); }

  console.log("\nAll unique addresses in transfers:");
  for (const addr of addrs) {
    const isPool = isPoolAddr(addr, poolMap);
    if (isPool) console.log(`  ${addr} -> IS POOL (in poolMap: ${poolMap.has(addr)})`);
  }

  console.log("\nChecking multiswap pool specifically:");
  console.log("  In addrs:", addrs.has(MULTISWAP_POOL));
  const incoming = transfers.filter(t => t.to === MULTISWAP_POOL && t.from !== ZERO_ADDR);
  const outgoing = transfers.filter(t => t.from === MULTISWAP_POOL && t.to !== ZERO_ADDR);
  console.log("  Incoming:", incoming.length, incoming.map(t => `${t.token.slice(0,10)} amt=${t.amount}`));
  console.log("  Outgoing:", outgoing.length, outgoing.map(t => `${t.token.slice(0,10)} amt=${t.amount}`));
}
main().catch(console.error);
