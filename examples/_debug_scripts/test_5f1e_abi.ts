import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });
const pool = "0x5f1e8ed8468232bab71eda9f4598bda3161f48ea";
const blockHex = `0x${80127543n.toString(16)}`;

// The pool was called by 0x29ed0a2f with selector 0xfe029156
// Let's see the actual calldata from the trace
// From the tx trace, the call was: 0x29ed0a2f → 0x5f1e8ed8 0xfe029156
// Let me get the full calldata from debug_traceTransaction

const resp = await (client.request as any)({
  method: "debug_traceTransaction",
  params: ["0x47e00934c4c69c5f47f3b03b5f7ce3070c983205bab36e63b751504dd488a83d", { tracer: "callTracer" }],
});

function findPool(c: any, target: string): any[] {
  const results: any[] = [];
  if ((c.to || '').toLowerCase() === target) results.push(c);
  for (const sub of (c.calls || [])) results.push(...findPool(sub, target));
  return results;
}

const calls = findPool(resp, pool);
for (const c of calls) {
  console.log(`Input: ${c.input?.slice(0, 200)}`);
  console.log(`Output: ${c.output?.slice(0, 200)}`);
  console.log(`Calls: ${c.calls?.length}`);
  for (const sub of (c.calls || [])) {
    console.log(`  sub: ${sub.type} ${sub.from?.slice(0,10)}→${sub.to?.slice(0,10)} ${sub.input?.slice(0,20)}`);
  }
}
