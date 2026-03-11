// Get full trace with no truncation
import { createPublicClient, http, decodeAbiParameters, encodeFunctionData, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { readFileSync } from "node:fs";
import { getBalanceOverride, getAllowanceOverride, ROUTER_ADDRESS } from "hayabusa-router";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const BYTECODE = "0x" + readFileSync("/home/claude/defi-toolbox-avalanche/router/contracts/bytecode.hex", "utf-8").trim();
const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const DUMMY = "0x000000000000000000000000000000000000dEaD";
const amountIn = 4950000000n;
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

const balOverride = getBalanceOverride(USDC, amountIn, DUMMY);
const allowOverride = getAllowanceOverride(USDC, DUMMY, ROUTER_ADDRESS);

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

const trace = await client.request({
  method: "debug_traceCall" as any,
  params: [
    { from: DUMMY, to: ROUTER_ADDRESS, data, gas: "0x989680" },
    blockHex,
    { tracer: "callTracer", tracerConfig: { withLog: true }, stateOverrides: stateOverride },
  ] as any,
}) as any;

function printTrace(call: any, depth = 0) {
  const indent = "  ".repeat(depth);
  const out = (call.output || "");
  const fullOut = out.length > 66 ? out.slice(0, 66) + "..." : out;
  console.log(`${indent}${call.type} ${(call.to||"").slice(0,12)} ${(call.input||"").slice(0,12)} out=${fullOut} ${call.error||""}`);
  for (const sub of (call.calls || [])) printTrace(sub, depth + 1);
}
printTrace(trace);
