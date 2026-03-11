// Test WooPP V2 WAVAX→USDC direction
import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { quoteRoute } from "hayabusa-router";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const txHash = "0x316fb254d0e8bb3f35b59e42a80f2e9f17d37e0b3f7c6cef025eef27cdc0e4ed";

// Get the real tx to understand its parameters
const trace = await client.request({
  method: "debug_traceTransaction" as any,
  params: [txHash, { tracer: "callTracer" }] as any,
}) as any;

// Find WooPP call
function findWooppCall(call: any): any {
  if ((call.to || "").toLowerCase() === "0xaba7ed514217d51630053d73d358ac2502d3f9bb" && call.input?.startsWith("0xac8bb7d9")) {
    return call;
  }
  for (const sub of (call.calls || [])) {
    const found = findWooppCall(sub);
    if (found) return found;
  }
  return null;
}

const wooppCall = findWooppCall(trace);
if (wooppCall) {
  console.log("WooPP call input:", wooppCall.input?.slice(0, 200));
  console.log("WooPP call from:", wooppCall.from);
  // Decode params
  const input = wooppCall.input;
  const direction = BigInt("0x" + input.slice(10 + 64, 10 + 128));
  const amount = BigInt("0x" + input.slice(10 + 128, 10 + 192));
  const minOutput = BigInt("0x" + input.slice(10 + 192, 10 + 256));
  console.log("direction:", direction, "(1=sell base = WAVAX→USDC)");
  console.log("amount:", amount);
  console.log("minOutput:", minOutput);
}

// Now get the tx receipt to see what block
const receipt = await client.request({
  method: "eth_getTransactionReceipt" as any,
  params: [txHash] as any,
}) as any;
const blockNumber = BigInt(receipt.blockNumber) - 1n;
console.log("\nBlock:", blockNumber);

// Load the payload to see what quoteRoute is called with
import { readFileSync } from "node:fs";
const payload = JSON.parse(readFileSync(`/home/claude/defi-toolbox-avalanche/examples/03_route_analyzer/payloads/${txHash}.json`, "utf-8"));
console.log("Payload route:", JSON.stringify(payload.route, null, 2));
console.log("amountIn:", payload.amountIn);
console.log("expected:", payload.amountOut);
