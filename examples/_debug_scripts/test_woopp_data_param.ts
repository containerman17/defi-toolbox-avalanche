// Check what 'data' parameter WooPP expects and if it matters
import { createPublicClient, http, encodeFunctionData, decodeAbiParameters, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { readFileSync } from "node:fs";
import { getBalanceOverride, getAllowanceOverride, ROUTER_ADDRESS } from "hayabusa-router";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const BYTECODE = "0x" + readFileSync("/home/claude/defi-toolbox-avalanche/router/contracts/bytecode.hex", "utf-8").trim();

const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const amountIn = 4950000000n;
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;
const DUMMY_SENDER = "0x000000000000000000000000000000000000dEaD";

// Get the REAL calldata from the real tx for WooPP call
const txHash = "0x1f84c56947f5d4e8d12096ea6c4622f368405b87f96f0924d8775453cea4bbff";
const realTrace = await client.request({
  method: "debug_traceTransaction" as any,
  params: [txHash, { tracer: "callTracer" }] as any,
}) as any;

function findWooppCall(call: any): any {
  if ((call.to || "").toLowerCase() === WOOPP.toLowerCase() && call.input?.startsWith("0xac8bb7d9")) {
    return call;
  }
  for (const sub of (call.calls || [])) {
    const found = findWooppCall(sub);
    if (found) return found;
  }
  return null;
}

const realWooppCall = findWooppCall(realTrace);
console.log("Real WooPP call input:", realWooppCall?.input);
console.log("Real WooPP call from:", realWooppCall?.from);
console.log("Real WooPP call output:", realWooppCall?.output?.slice(0, 66));

// Decode the real calldata
if (realWooppCall?.input) {
  const data = ("0x" + realWooppCall.input.slice(10)) as Hex;
  try {
    // swap(address broker, uint256 direction, uint256 amount, uint256 minOutput, bytes data)
    // But data is ABI encoded, so we need to handle the dynamic bytes
    const [broker, direction, amount, minOutput] = decodeAbiParameters(
      [{type:"address"},{type:"uint256"},{type:"uint256"},{type:"uint256"}],
      data
    );
    console.log("\nDecoded real WooPP call:");
    console.log("  broker:", broker);
    console.log("  direction:", direction);
    console.log("  amount:", amount);
    console.log("  minOutput:", minOutput);
    console.log("  raw bytes after first 4 params:", data.slice(2 + 64*4));
  } catch(e: any) {
    console.log("Decode error:", e.message);
  }
}

// Build state override
const balOverride = getBalanceOverride(USDC, amountIn, DUMMY_SENDER);
const allowOverride = getAllowanceOverride(USDC, DUMMY_SENDER, ROUTER_ADDRESS);
const merged: Record<string, Record<string, Hex>> = {};
for (const ovr of [balOverride, allowOverride]) {
  for (const [addr, val] of Object.entries(ovr)) {
    if (!merged[addr]) merged[addr] = {};
    Object.assign(merged[addr], val.stateDiff);
  }
}
const stateOverride: Record<string, any> = {};
for (const [address, slots] of Object.entries(merged)) {
  stateOverride[address] = { stateDiff: slots };
}
stateOverride[ROUTER_ADDRESS] = { code: BYTECODE as Hex };

// Try the exact real calldata but with ROUTER_ADDRESS as broker
// Real data field: USDC address
const realDataField = USDC; // from real tx decoding

// Try 3 different approaches for the 'data' parameter:
// 1. Our current: data = USDC (from main bug fix)
// 2. Real tx: also USDC
// 3. Try empty bytes: data = ""

for (const [name, dataParam] of [
  ["data=USDC", USDC],
  ["data=WAVAX", WAVAX],
  ["data=empty", ""],
] as [string, string][]) {
  let callData: string;
  if (dataParam === "") {
    callData = "0xac8bb7d9" +
      ROUTER_ADDRESS.slice(2).padStart(64, "0") +
      "0".padStart(64, "0") +
      amountIn.toString(16).padStart(64, "0") +
      "0".padStart(64, "0") +
      "80".padStart(64, "0") + // offset for bytes (4th param at 0x80 = 128 bytes)
      "0".padStart(64, "0"); // empty bytes length
  } else {
    callData = "0xac8bb7d9" +
      ROUTER_ADDRESS.slice(2).padStart(64, "0") +
      "0".padStart(64, "0") +
      amountIn.toString(16).padStart(64, "0") +
      "0".padStart(64, "0") +
      "a0".padStart(64, "0") + // offset = 0xa0 = 160
      "20".padStart(64, "0") + // length = 32 bytes
      dataParam.slice(2).padStart(64, "0"); // token address
  }

  // Trace this call
  const trace = await client.request({
    method: "debug_traceCall" as any,
    params: [
      { from: ROUTER_ADDRESS, to: WOOPP, data: callData, gas: "0x989680" },
      blockHex,
      { tracer: "callTracer", stateOverrides: stateOverride },
    ] as any,
  }) as any;

  function hasWavaxTransfer(call: any): boolean {
    if ((call.to || "").toLowerCase() === WAVAX.toLowerCase() && call.input?.startsWith("0xa9059cbb")) return true;
    for (const sub of (call.calls || [])) if (hasWavaxTransfer(sub)) return true;
    return false;
  }

  const wavaxSent = hasWavaxTransfer(trace);
  console.log(`\n${name}: error=${trace.error || "none"}, WAVAX transferred=${wavaxSent}, output=${trace.output?.slice(0, 66)}`);
}
