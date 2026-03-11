import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const ORACLE = "0xd92e3c8f60a91fa6dfc59a43f7e1f7e43ee56be4";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";

const block = BigInt(80108339);
const blockMinus1 = block - 1n;

// Let's look at the oracle state function that WooPP calls: 0xc1701b67
const data = "0xc1701b67000000000000000000000000b31f66aa3c1e785363f0875a1b74e27b85fd66c7";

for (const [label, bHex] of [
  ["block-2", `0x${(block-2n).toString(16)}`],
  ["block-1", `0x${(block-1n).toString(16)}`],
  ["block", `0x${block.toString(16)}`],
  ["latest", "latest"],
]) {
  try {
    const result = await client.request({
      method: "eth_call" as any,
      params: [{ to: ORACLE, data }, bHex] as any,
    });
    console.log(`${label}: ${result || "(empty)"}`);
  } catch (e: any) {
    console.log(`${label}: revert - ${e.message?.slice(0, 80)}`);
  }
}

// Also check debug_traceCall of the actual tx to see what the oracle returns in context
const traceResult = await client.request({
  method: "debug_traceTransaction" as any,
  params: ["0x1f84c56947f5d4e8d12096ea6c4622f368405b87f96f0924d8775453cea4bbff", { tracer: "callTracer" }] as any,
}) as any;

// Find the oracle call in the trace
function findCallsTo(call: any, address: string, results: any[] = []) {
  if (call.to?.toLowerCase() === address.toLowerCase()) {
    results.push(call);
  }
  for (const sub of (call.calls || [])) findCallsTo(sub, address, results);
  return results;
}

const oracleCalls = findCallsTo(traceResult, ORACLE);
console.log(`\nOracle calls in actual tx (${oracleCalls.length}):`);
for (const c of oracleCalls.slice(0, 3)) {
  console.log(`  ${c.type} input=${c.input?.slice(0,10)} output=${c.output?.slice(0,66)}`);
}
