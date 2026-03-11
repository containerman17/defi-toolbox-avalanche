import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";

const trace = await client.request({
  method: "debug_traceTransaction" as any,
  params: ["0x1f84c56947f5d4e8d12096ea6c4622f368405b87f96f0924d8775453cea4bbff", { tracer: "callTracer" }] as any,
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
  console.log(`Full input: ${fullInput}`);
  
  if (fullInput.startsWith("0xac8bb7d9") && fullInput.length >= 10 + 6*64) {
    const args = fullInput.slice(10); // Remove selector
    const broker = "0x" + args.slice(24, 64);
    const direction = BigInt("0x" + args.slice(64, 128));
    const amount = BigInt("0x" + args.slice(128, 192));
    const minOutput = "0x" + args.slice(192, 256);
    // Skip offset (256-320) and length (320-384)
    const dataField = "0x" + args.slice(384, 448);
    
    console.log(`  broker: ${broker}`);
    console.log(`  direction: ${direction} (${direction === 0n ? "sell quote (USDC->X)" : "sell base (X->USDC)"})`);
    console.log(`  amount: ${amount}`);
    console.log(`  minOutput (raw): ${minOutput}`);
    console.log(`  data (token): ${dataField}`);
  }
}
