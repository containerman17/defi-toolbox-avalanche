// Extract exact callback data from real WooPP transaction
import { createPublicClient, http, decodeAbiParameters, type Hex } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const txHash = "0x1f84c56947f5d4e8d12096ea6c4622f368405b87f96f0924d8775453cea4bbff";
const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";

// Get call trace of real tx to see callback data
const trace = await client.request({
  method: "debug_traceTransaction" as any,
  params: [txHash, { tracer: "callTracer", tracerConfig: { withLog: false } }] as any,
}) as any;

// Find the WooPP callback call (from WooPP to LFJ aggregator)
function findCallbacks(call: any, depth = 0): void {
  if ((call.from || "").toLowerCase() === WOOPP) {
    console.log(`${"  ".repeat(depth)}WooPP callback: to=${call.to} input=${call.input?.slice(0, 200)} output=${call.output?.slice(0, 34)}`);
    if (call.input?.startsWith("0xc3251075")) {
      console.log("  Found callback! Decoding...");
      const inputHex = call.input.slice(2) as Hex;
      // Skip 4-byte selector
      const params = ("0x" + inputHex.slice(8)) as Hex;
      try {
        const [toAmount, fromAmount, dataOffset, dataLength, baseToken] = decodeAbiParameters(
          [{type:"int256"},{type:"uint256"},{type:"uint256"},{type:"uint256"},{type:"bytes32"}],
          params
        );
        console.log("  toAmount (int256):", toAmount);
        console.log("  fromAmount (uint256):", fromAmount);
        console.log("  dataOffset:", dataOffset);
        console.log("  dataLength:", dataLength);
        console.log("  baseToken (raw bytes32):", baseToken);
      } catch(e) {
        // Try different decoding
        const [toAmount, fromAmount] = decodeAbiParameters(
          [{type:"int256"},{type:"uint256"}],
          params
        );
        console.log("  toAmount:", toAmount);
        console.log("  fromAmount:", fromAmount);
      }
    }
  }
  for (const sub of (call.calls || [])) findCallbacks(sub, depth + 1);
}

console.log("=== Real tx callback data ===");
findCallbacks(trace);

// Also check what transfers happened in WooPP step (step 0)
// Find WooPP calls in the main trace
function printWooppCalls(call: any, depth = 0): void {
  const from = (call.from || "").toLowerCase();
  const to = (call.to || "").toLowerCase();
  if (from === WOOPP || to === WOOPP) {
    console.log(`${"  ".repeat(depth)}${call.type} ${call.from?.slice(0,12)}→${call.to?.slice(0,12)} in=${call.input?.slice(0,20)} out=${call.output?.slice(0,66)} ${call.error||""}`);
    for (const sub of (call.calls || [])) printWooppCalls(sub, depth + 1);
  } else {
    for (const sub of (call.calls || [])) printWooppCalls(sub, depth + 1);
  }
}

console.log("\n=== All WooPP-related calls in real tx ===");
printWooppCalls(trace);
