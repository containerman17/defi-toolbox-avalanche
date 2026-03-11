// Call trace of WooPP V2 simulation to see ALL calls
import { createPublicClient, http, encodeFunctionData, type Hex } from "viem";
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

const swapAbi = [{
  name: "swap",
  type: "function",
  inputs: [
    { name: "pools", type: "address[]" },
    { name: "poolTypes", type: "uint8[]" },
    { name: "tokens", type: "address[]" },
    { name: "amountIn", type: "uint256" },
    { name: "extraDatas", type: "bytes[]" },
  ],
  outputs: [{ type: "uint256" }],
}] as const;

const data = encodeFunctionData({
  abi: swapAbi,
  functionName: "swap",
  args: [[WOOPP as `0x${string}`], [14], [USDC as `0x${string}`, WAVAX as `0x${string}`], amountIn, ["0x"]],
});

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
stateOverride[ROUTER_ADDRESS] = {
  ...(stateOverride[ROUTER_ADDRESS] ?? {}),
  code: BYTECODE as Hex,
};

console.log("State override addresses:", Object.keys(stateOverride));

const trace = await client.request({
  method: "debug_traceCall" as any,
  params: [
    { from: DUMMY_SENDER, to: ROUTER_ADDRESS, data, gas: "0x989680" },
    blockHex,
    { tracer: "callTracer", tracerConfig: { withLog: true }, stateOverrides: stateOverride },
  ] as any,
}) as any;

function printTrace(call: any, depth = 0) {
  const indent = "  ".repeat(depth);
  const from = (call.from || "").slice(0, 12);
  const to = (call.to || "").slice(0, 12);
  const input = (call.input || "").slice(0, 18);
  const output = (call.output || "").slice(0, 34);
  const value = call.value ? ` val=${call.value}` : "";
  const gas = call.gasUsed ? ` gas=${call.gasUsed}` : "";
  const err = call.error ? ` ERROR:${call.error}` : "";
  console.log(`${indent}${call.type} ${from}→${to} ${input}${value}${gas}${err}`);
  if (call.output && call.output !== "0x") {
    console.log(`${indent}  → output: ${output}`);
  }
  for (const log of (call.logs || [])) {
    const topics = (log.topics || []).map((t: string) => t.slice(0, 10)).join(",");
    console.log(`${indent}  LOG ${(log.address || "").slice(0, 12)} topics=${topics} data=${(log.data || "").slice(0, 34)}`);
  }
  for (const sub of (call.calls || [])) printTrace(sub, depth + 1);
}

console.log("\n=== Call trace ===");
printTrace(trace);
console.log("\nTop-level error:", trace.error);
console.log("Top-level output:", trace.output?.slice(0, 66));
