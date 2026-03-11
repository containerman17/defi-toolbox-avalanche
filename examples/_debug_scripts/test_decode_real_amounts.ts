import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const LFJ = "0x63242a4ea82847b20e506b63b0e2e2eff0cc6cb0";

const trace = await client.request({
  method: "debug_traceTransaction" as any,
  params: ["0x1f84c56947f5d4e8d12096ea6c4622f368405b87f96f0924d8775453cea4bbff", { tracer: "callTracer" }] as any,
}) as any;

function findCallsFrom(call: any, addr: string, results: any[] = []) {
  if ((call.from || "").toLowerCase() === addr.toLowerCase()) results.push(call);
  for (const sub of (call.calls || [])) findCallsFrom(sub, addr, results);
  return results;
}

const fromWoopp = findCallsFrom(trace, WOOPP);
console.log("All calls FROM WooPP:");
for (const c of fromWoopp) {
  const input = c.input || "";
  const sel = input.slice(0, 10);
  const full = input.length;
  
  if (sel === "0xa9059cbb" && c.to?.toLowerCase() === WAVAX.toLowerCase()) {
    // transfer(to, amount)
    const to = "0x" + input.slice(34, 74);
    const amount = BigInt("0x" + input.slice(74, 138));
    console.log(`  WAVAX.transfer(to=${to.slice(0,12)}, amount=${amount})`);
  } else if (sel === "0x70a08231") {
    const who = "0x" + input.slice(34, 74);
    const result = c.output ? BigInt(c.output) : "unknown";
    console.log(`  USDC.balanceOf(${who.slice(0,12)}) = ${result}`);
  } else if (sel === "0xc3251075") {
    // callback: read fromAmount (word2)
    const fromAmount = input.length >= 10 + 128 ? BigInt("0x" + input.slice(10 + 64, 10 + 128)) : "unknown";
    console.log(`  CALLBACK c3251075, fromAmount=${fromAmount}, full=${input.slice(0, 140)}`);
  } else {
    console.log(`  to=${(c.to||"").slice(0,12)} sel=${sel} len=${full}`);
  }
}

// Now check: what USDC amount was sent by LFJ to WooPP BEFORE the swap call
function findCallsTo(call: any, addr: string, results: any[] = []) {
  if ((call.to || "").toLowerCase() === addr.toLowerCase()) results.push(call);
  for (const sub of (call.calls || [])) findCallsTo(sub, addr, results);
  return results;
}

console.log("\nAll calls TO WooPP:");
const toWoopp = findCallsTo(trace, WOOPP);
for (const c of toWoopp) {
  const input = c.input || "";
  const sel = input.slice(0, 10);
  console.log(`  from=${(c.from||"").slice(0,12)} sel=${sel} len=${input.length}`);
}
