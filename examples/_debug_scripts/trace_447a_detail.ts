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
  
  // Find all calls
  function findCalls(call: any, depth = 0): any[] {
    const calls: any[] = [];
    calls.push({ ...call, _depth: depth });
    if (call.calls) {
      for (const sub of call.calls) {
        calls.push(...findCalls(sub, depth + 1));
      }
    }
    return calls;
  }
  
  const calls = findCalls(trace);
  
  // Show calls involving 0xa196a03653f6cc5ca0282a8bd7ec60e93f620afc or 0xed2a7edd7413021d440b09d654f3b87712abab66
  const targets = ["0xa196a03653f6cc5ca0282a8bd7ec60e93f620afc", "0xed2a7edd7413021d440b09d654f3b87712abab66"];
  
  console.log("Calls to/from target pools:");
  for (const call of calls) {
    if (targets.includes(call.to?.toLowerCase()) || targets.includes(call.from?.toLowerCase())) {
      console.log(`Depth ${call._depth}: ${call.from?.slice(0,10)} -> ${call.to?.slice(0,10)} | Method: ${call.input?.slice(0,10)} | ${call.type}`);
    }
  }
}

main().catch(console.error);
