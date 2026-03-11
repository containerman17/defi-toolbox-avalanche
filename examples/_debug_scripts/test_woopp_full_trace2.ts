import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

// Get full trace of the original tx
const trace = await client.request({
  method: "debug_traceTransaction" as any,
  params: ["0x1f84c56947f5d4e8d12096ea6c4622f368405b87f96f0924d8775453cea4bbff", { tracer: "callTracer" }] as any,
}) as any;

// Find all calls that go TO WooPP
function findCallsTo(call: any, addr: string, results: any[] = [], parentOutput = "") {
  if ((call.to || "").toLowerCase() === addr.toLowerCase()) {
    results.push({ ...call, parentOutput });
  }
  for (const sub of (call.calls || [])) {
    findCallsTo(sub, addr, results, call.output || "");
  }
  return results;
}

// Find calls FROM WooPP 
function findCallsFrom(call: any, addr: string, results: any[] = []) {
  if ((call.from || "").toLowerCase() === addr.toLowerCase()) {
    results.push(call);
  }
  for (const sub of (call.calls || [])) findCallsFrom(sub, addr, results);
  return results;
}

const wooToCall = findCallsTo(trace, WOOPP);
console.log(`Calls TO WooPP: ${wooToCall.length}`);
for (const c of wooToCall) {
  console.log(`  input=${c.input?.slice(0,20)} output=${c.output?.slice(0,68)} error=${c.error || ""}`);
}

const wooFromCalls = findCallsFrom(trace, WOOPP);
console.log(`\nCalls FROM WooPP: ${wooFromCalls.length}`);
for (const c of wooFromCalls) {
  console.log(`  to=${c.to} input=${c.input?.slice(0,20)} output=${c.output?.slice(0,68)} error=${c.error || ""}`);
}
