// Direct call to WooPP.swap() with our router having the callback bytecode
import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { readFileSync } from "node:fs";
import { keccak256, encodeAbiParameters } from "viem";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const ROUTER = "0x2bef1becdafcfe8990a233d03a98bbb39021c96e";
const amountIn = 4950000000n;
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

const bytecode = "0x" + readFileSync("/home/claude/defi-toolbox-avalanche/router/contracts/bytecode.hex", "utf-8").trim();
const slot = keccak256(encodeAbiParameters([{type: "address"}, {type: "uint256"}], [ROUTER as Hex, 9n]));

// Call WooPP.swap directly from the router
const swapCalldata = "0xac8bb7d9" + 
  ROUTER.slice(2).padStart(64, "0") +
  "0".padStart(64, "0") +
  amountIn.toString(16).padStart(64, "0") +
  "0".padStart(64, "0") +
  "00000000000000000000000000000000000000000000000000000000000000a0" +
  "0000000000000000000000000000000000000000000000000000000000000020" +
  WAVAX.slice(2).padStart(64, "0");

const stateOverride = {
  [ROUTER]: { code: bytecode as Hex, stateDiff: { [slot]: `0x${amountIn.toString(16).padStart(64, "0")}` as Hex } },
  [USDC]: { stateDiff: { [slot]: `0x${amountIn.toString(16).padStart(64, "0")}` as Hex } },
};

// Call WooPP.swap with from=ROUTER (so the callback goes to router)
const result = await client.request({
  method: "eth_call" as any,
  params: [
    { from: ROUTER, to: WOOPP, data: swapCalldata },
    blockHex,
    stateOverride,
  ] as any,
}).catch(e => "err: " + e.message?.slice(0, 200));
console.log("WooPP.swap direct from router:", result);

// Also trace it
const trace = await client.request({
  method: "debug_traceCall" as any,
  params: [
    { from: ROUTER, to: WOOPP, data: swapCalldata },
    blockHex,
    { tracer: "callTracer", stateOverrides: stateOverride },
  ] as any,
}) as any;
console.log("\nTrace:");
function printTrace(call: any, depth = 0) {
  const indent = "  ".repeat(depth);
  console.log(`${indent}${call.type} ${(call.from||"").slice(0,10)}→${(call.to||"").slice(0,10)} ${(call.input||"").slice(0,10)} out=${(call.output||"").slice(0,34)} ${call.error||""}`);
  for (const sub of (call.calls||[])) printTrace(sub, depth+1);
}
printTrace(trace);
