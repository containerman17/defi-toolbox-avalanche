import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const RPC_URL = process.env.RPC_URL || "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(RPC_URL) });

const TX_HASH = "0x447a7f6aca1d7593abec732829422fb0ab3bce338f6b628945cb0f02f6f7cc2c";

async function main() {
  const trace = await (client as any).request({
    method: "debug_traceTransaction",
    params: [TX_HASH, { tracer: "callTracer", tracerConfig: { withLog: true } }]
  });
  
  const TRANSFER_TOPIC = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef";
  
  function findLogs(call: any, depth = 0): any[] {
    const logs: any[] = [];
    if (call.logs) {
      for (const log of call.logs) {
        if (log.topics && log.topics[0] === TRANSFER_TOPIC) {
          logs.push({ ...log, _from: call.to, depth });
        }
      }
    }
    if (call.calls) {
      for (const sub of call.calls) {
        logs.push(...findLogs(sub, depth + 1));
      }
    }
    return logs;
  }
  
  const logs = findLogs(trace);
  
  // Token addresses
  const TOKEN_NAMES: Record<string, string> = {
    "0x5947bb275c521040051d82396192181b413227a3": "LINK",
    "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e": "USDC",
    "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664": "USDC.e",
    "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7": "USDt",
    "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7": "WAVAX",
  };
  
  const name = (addr: string) => TOKEN_NAMES[addr.toLowerCase()] || addr.slice(0, 10);
  
  console.log("All transfers:");
  for (const log of logs) {
    if (log.topics.length >= 3) {
      const from = "0x" + log.topics[1].slice(26).toLowerCase();
      const to = "0x" + log.topics[2].slice(26).toLowerCase();
      const amount = BigInt(log.data || "0x0");
      console.log(`${name(log.address.toLowerCase())} | From: ${from} | To: ${to} | Amount: ${amount}`);
    }
  }
  
  // Find all unique recipient contracts (pool addresses)
  const receivers = new Map<string, number>();
  for (const log of logs) {
    if (log.topics.length >= 3) {
      const to = "0x" + log.topics[2].slice(26).toLowerCase();
      if (!["0x0000000000000000000000000000000000000000"].includes(to)) {
        receivers.set(to, (receivers.get(to) || 0) + 1);
      }
    }
  }
  
  console.log("\nUnique receiver addresses (potential pools):");
  for (const [addr, count] of receivers) {
    console.log(`${addr} (${count} receives)`);
  }
}

main().catch(console.error);
