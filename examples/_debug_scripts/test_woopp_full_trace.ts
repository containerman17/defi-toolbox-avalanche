import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const txHash = "0x1f84c56947f5d4e8d12096ea6c4622f368405b87f96f0924d8775453cea4bbff";
const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";

const traceResult = await client.request({
  method: "debug_traceTransaction" as any,
  params: [txHash, { tracer: "callTracer" }] as any,
}) as any;

// Find the WooPP call and print ALL addresses involved
function findCallsTo(call: any, address: string): any[] {
  const result = [];
  if (call.to?.toLowerCase() === address) result.push(call);
  for (const sub of (call.calls || [])) result.push(...findCallsTo(sub, address));
  return result;
}

const wooppCalls = findCallsTo(traceResult, WOOPP);
for (const call of wooppCalls) {
  // Get all unique addresses called
  const addrs = new Set<string>();
  function collectAddrs(c: any) {
    if (c.to) addrs.add(c.to);
    for (const sub of (c.calls || [])) collectAddrs(sub);
  }
  collectAddrs(call);
  console.log("Addresses called from WooPP:");
  for (const a of addrs) console.log(" ", a);
}
