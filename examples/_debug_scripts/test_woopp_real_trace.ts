// Trace real WooPP V2 transaction to see what happens
import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const txHash = "0x1f84c56947f5d4e8d12096ea6c4622f368405b87f96f0924d8775453cea4bbff";

const traceResult = await client.request({
  method: "debug_traceTransaction" as any,
  params: [
    txHash,
    { tracer: "callTracer" }
  ] as any,
});

function printTrace(call: any, depth = 0) {
  const indent = "  ".repeat(depth);
  const from = (call.from || "").slice(0, 10);
  const to = (call.to || "").slice(0, 10);
  const input = (call.input || "").slice(0, 10);
  const value = call.value ? ` val=${BigInt(call.value)}` : "";
  const output = call.output ? ` out=${call.output.slice(0, 18)}` : "";
  const error = call.error ? ` ERROR=${call.error}` : "";
  console.log(`${indent}${call.type} ${from}→${to} ${input}${value}${output}${error}`);
  for (const sub of (call.calls || [])) {
    printTrace(sub, depth + 1);
  }
}

// Find the WooPP V2 call in the first step of the split
const split = (traceResult as any).calls?.[0];
console.log("Looking for WooPP V2 leg in split route...");

// The real tx goes through LFJ router, find the WooPP call
function findCalls(call: any, selector: string, depth = 0): any[] {
  const result = [];
  if (call.input?.startsWith(selector)) {
    result.push({ call, depth });
  }
  for (const sub of (call.calls || [])) {
    result.push(...findCalls(sub, selector, depth + 1));
  }
  return result;
}

// Find calls to WOOPP
function findCallsTo(call: any, address: string, depth = 0): any[] {
  const result = [];
  if (call.to?.toLowerCase() === address.toLowerCase()) {
    result.push({ call, depth });
  }
  for (const sub of (call.calls || [])) {
    result.push(...findCallsTo(sub, address, depth + 1));
  }
  return result;
}

const wooppCalls = findCallsTo(traceResult, "0xaba7ed514217d51630053d73d358ac2502d3f9bb");
console.log(`Found ${wooppCalls.length} calls to WooPP`);
for (const { call, depth } of wooppCalls) {
  console.log(`\nWooPP call (depth ${depth}):`);
  printTrace(call, 0);
}
