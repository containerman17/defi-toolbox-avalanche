import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const txHash = "0x1f84c56947f5d4e8d12096ea6c4622f368405b87f96f0924d8775453cea4bbff";

const trace = await client.request({
  method: "debug_traceTransaction" as any,
  params: [txHash, { tracer: "callTracer" }] as any,
}) as any;

function findCallsTo(call: any, addr: string): any[] {
  const result = [];
  if (call.to?.toLowerCase() === addr) result.push(call);
  for (const sub of (call.calls||[])) result.push(...findCallsTo(sub, addr));
  return result;
}

const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const calls = findCallsTo(trace, WOOPP);
for (const call of calls) {
  const input = call.input;
  console.log("Full calldata hex:");
  console.log(input);
  console.log("Length:", (input.length - 2) / 2, "bytes");
  
  // Parse precisely  
  const data = input.slice(10); // remove selector
  for (let i = 0; i < data.length; i += 64) {
    const word = data.slice(i, i + 64);
    const idx = i / 64;
    console.log(`  [${idx}]: 0x${word} (${BigInt("0x" + word)})`);
  }
}
