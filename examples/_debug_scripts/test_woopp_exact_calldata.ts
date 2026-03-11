// Test WooPP with exact real calldata (replace broker with ROUTER)
import { createPublicClient, http, type Hex } from "viem";
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

// The real tx calldata with LFJ aggregator as broker:
// 0xac8bb7d9
//   broker=0x63242A4Ea82847b20E506b63B0e2e2eFF0CC6cB0
//   direction=0
//   amount=4950000000
//   minOutput=uint128.max=340282366920938463463374607431768211455
//   data=USDC

// Use exact same calldata but with ROUTER_ADDRESS as broker
const exactCalldata = "0xac8bb7d9" +
  ROUTER_ADDRESS.slice(2).padStart(64, "0") + // broker = ROUTER
  "0".padStart(64, "0") + // direction = 0
  amountIn.toString(16).padStart(64, "0") + // amount = 4950000000
  "ffffffffffffffffffffffffffffffff".padStart(64, "0") + // minOutput = uint128.max
  "a0".padStart(64, "0") + // bytes offset
  "20".padStart(64, "0") + // bytes length = 32
  USDC.slice(2).padStart(64, "0"); // data = USDC

console.log("Using minOutput=uint128.max from real tx");

// Test direct WooPP call
const trace = await client.request({
  method: "debug_traceCall" as any,
  params: [
    { from: ROUTER_ADDRESS, to: WOOPP, data: exactCalldata, gas: "0x989680" },
    blockHex,
    { tracer: "callTracer", stateOverrides: stateOverride },
  ] as any,
}) as any;

function hasWavaxTransfer(call: any): boolean {
  if ((call.to || "").toLowerCase() === WAVAX.toLowerCase() && call.input?.startsWith("0xa9059cbb")) return true;
  for (const sub of (call.calls || [])) if (hasWavaxTransfer(sub)) return true;
  return false;
}

function printTrace(call: any, depth = 0) {
  const indent = "  ".repeat(depth);
  const isWavax = (call.to || "").toLowerCase() === WAVAX.toLowerCase();
  const err = call.error ? ` ERR:${call.error}` : "";
  console.log(`${indent}${call.type} ${call.from?.slice(0,10)}→${call.to?.slice(0,10)} ${(call.input||"").slice(0,10)}${err}${isWavax ? " ===WAVAX===" : ""}`);
  if (call.output) console.log(`${indent}  out=${call.output.slice(0, 66)}`);
  for (const sub of (call.calls || [])) printTrace(sub, depth + 1);
}

console.log("\nTrace:");
printTrace(trace);
console.log("\nWAVAX transferred:", hasWavaxTransfer(trace));
console.log("Error:", trace.error || "none");
console.log("Output:", trace.output?.slice(0, 66));
