import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";

const trace = await client.request({
  method: "debug_traceTransaction" as any,
  params: ["0x316fb254e2f34a0754c689c4c8b794f200a2ceb0249337bfd2ba52dc2c680a69", { tracer: "callTracer" }] as any,
}) as any;

function findCallsTo(call: any, addr: string, results: any[] = []) {
  if ((call.to || "").toLowerCase() === addr.toLowerCase()) results.push(call);
  for (const sub of (call.calls || [])) findCallsTo(sub, addr, results);
  return results;
}

const woopCalls = findCallsTo(trace, WOOPP);
console.log(`Calls to WooPP: ${woopCalls.length}`);
for (const c of woopCalls) {
  const fullInput = c.input || "";
  console.log(`Input: ${fullInput}`);
  
  // Decode: selector(4) + broker(32) + direction(32) + amount(32) + minOutput(32) + offset(32) + length(32) + data(32)
  if (fullInput.startsWith("0xac8bb7d9") && fullInput.length >= 10 + 14*32) {
    const args = fullInput.slice(10); // Remove selector
    const broker = "0x" + args.slice(24, 64);
    const direction = BigInt("0x" + args.slice(64, 128));
    const amount = BigInt("0x" + args.slice(128, 192));
    const minOutput = "0x" + args.slice(192, 256);
    const dataField = "0x" + args.slice(384, 448); // data content (after offset+length)
    
    console.log(`  broker: ${broker}`);
    console.log(`  direction: ${direction} (${direction === 0n ? "sell quote (USDC->X)" : "sell base (X->USDC)"})`);
    console.log(`  amount: ${amount}`);
    console.log(`  data (token): ${dataField}`);
  }
}
