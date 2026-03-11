import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";

const TRANSFER_TOPIC = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef";
const KNOWN: Record<string, string> = {
  "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7": "WAVAX",
  "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e": "USDC",
  "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7": "USDt",
  "0x49d5c2bdffac6ce2bfdb6640f4f80f226bc10bab": "WETH.e",
  "0x5520385bfcf07ec87c4c53a7d8d65595dff69fa4": "WooPP",
  "0x6cd2c4c74125a6ee1999a061b1cea9892e331339": "VaporDex",
  "0x5f1e8ed8468232bab71eda9f4598bda3161f48ea": "Multiswap",
  "0x152b9d0fdc40c096757f570a51e494bd4b943e50": "BridgeTok",
  "0x29ed0a2f22a92ff84a7f196785ca6b0d21aeec62": "Executor",
  "0x0000000000000000000000000000000000000000": "ZERO",
};
function tok(a: string) { return KNOWN[a.toLowerCase()] ?? a.slice(0, 10); }

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

async function main() {
  const tx = await client.getTransaction({ hash: "0xf2c2d7db1532c3ae9d38650753f776ace15b287e6e8bb2d6ccf2496f11e7bc71" as Hex });
  const parentBlock = `0x${(80141929n - 1n).toString(16)}`;
  const trace: any = await client.request({
    method: "debug_traceCall" as any,
    params: [{
      from: tx.from, to: tx.to, data: tx.input,
      value: tx.value ? `0x${tx.value.toString(16)}` : undefined,
      gas: "0x1C9C380",
    }, parentBlock, { tracer: "callTracer", tracerConfig: { withLog: true } }] as any,
  });

  function collectLogs(call: any): any[] {
    const logs: any[] = [];
    if (call.logs) for (const log of call.logs) logs.push({ address: log.address.toLowerCase(), topics: log.topics, data: log.data });
    if (call.calls) for (const sub of call.calls) logs.push(...collectLogs(sub));
    return logs;
  }

  const logs = collectLogs(trace);
  
  // Show all transfers with index, focusing on WooPP interactions
  console.log("Transfer events involving WooPP or related contracts:");
  let idx = 0;
  for (const log of logs) {
    if (log.topics[0] === TRANSFER_TOPIC && log.topics.length >= 3) {
      const token = tok(log.address);
      const from = tok(("0x" + log.topics[1].slice(26)).toLowerCase());
      const to = tok(("0x" + log.topics[2].slice(26)).toLowerCase());
      const fromAddr = ("0x" + log.topics[1].slice(26)).toLowerCase();
      const toAddr = ("0x" + log.topics[2].slice(26)).toLowerCase();
      const amount = BigInt(log.data);
      // Show if WooPP, Multiswap, BridgeTok, VaporDex involved
      if ([fromAddr, toAddr].some(a => ["0x5520385bfcf07ec87c4c53a7d8d65595dff69fa4", "0x5f1e8ed8468232bab71eda9f4598bda3161f48ea", "0x152b9d0fdc40c096757f570a51e494bd4b943e50", "0x6cd2c4c74125a6ee1999a061b1cea9892e331339", "0x29ed0a2f22a92ff84a7f196785ca6b0d21aeec62"].includes(a))) {
        console.log(`  [${idx}] ${token}: ${from} → ${to}  amt=${amount}`);
      }
    }
    idx++;
  }
}
main().catch(console.error);
