import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

// From the real tx trace, the WooPP callback to LFJ aggregator
// input = 0xc3251075ffffffffff...
// Let me get the full callback input

const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const LFJ_AGG = "0x63242a4ea82847b20e506b63b0e2e2eff0cc6cb0";

const trace = await client.request({
  method: "debug_traceTransaction" as any,
  params: ["0x1f84c56947f5d4e8d12096ea6c4622f368405b87f96f0924d8775453cea4bbff", { tracer: "callTracer" }] as any,
}) as any;

function findCallsFromTo(call: any, from: string, to: string, results: any[] = []) {
  if ((call.from || "").toLowerCase() === from.toLowerCase() &&
      (call.to || "").toLowerCase() === to.toLowerCase()) {
    results.push(call);
  }
  for (const sub of (call.calls || [])) findCallsFromTo(sub, from, to, results);
  return results;
}

// Find WooPP->LFJ aggregator callback
const callbacks = findCallsFromTo(trace, WOOPP, LFJ_AGG);
console.log(`WooPP→LFJ callbacks: ${callbacks.length}`);
for (const c of callbacks) {
  console.log(`  full input: ${c.input}`);
  
  const input = c.input || "";
  if (input.length >= 10 + 64) {
    const selector = input.slice(0, 10);
    const word1 = input.slice(10, 74);
    console.log(`  selector: ${selector}`);
    console.log(`  word1: ${word1}`);
    console.log(`  word1 as int256: ${BigInt("0x" + word1)}`);
    
    // In Solidity: assembly { amount := calldataload(4) }
    // calldataload(4) reads 32 bytes starting at offset 4 in calldata
    // So amount = the 32 bytes starting at position 4
    // calldata = [selector(4)] [word1(32)] ...
    // calldataload(4) = word1
    const amount = BigInt("0x" + word1);
    console.log(`  amount from calldataload(4): ${amount}`);
  }
}
