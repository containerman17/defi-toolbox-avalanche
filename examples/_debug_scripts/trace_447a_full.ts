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
  
  // Show all calls, simplified
  function showCalls(call: any, depth = 0, indent = "") {
    const from = call.from?.slice(0, 10) || "?";
    const to = call.to?.slice(0, 10) || "?";
    const sel = call.input?.slice(0, 10) || "?";
    const type = call.type || "?";
    
    // Show transfers in this call
    const transfers: string[] = [];
    if (call.logs) {
      for (const log of call.logs) {
        if (log.topics && log.topics[0] === TRANSFER_TOPIC) {
          const tfrom = "0x" + log.topics[1].slice(26);
          const tto = "0x" + log.topics[2].slice(26);
          const amount = BigInt(log.data || "0x0");
          transfers.push(`${log.address.slice(0,10)} ${tfrom.slice(0,10)}→${tto.slice(0,10)} ${amount}`);
        }
      }
    }
    
    const tline = transfers.length > 0 ? ` [${transfers.join(", ")}]` : "";
    console.log(`${indent}${type} ${from}→${to} sel=${sel}${tline}`);
    
    if (depth < 5 && call.calls) {
      for (const sub of call.calls) {
        showCalls(sub, depth + 1, indent + "  ");
      }
    }
  }
  
  showCalls(trace);
}

main().catch(console.error);
