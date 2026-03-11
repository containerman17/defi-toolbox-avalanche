// Debug WooPP V2 simulation with debug_traceCall
import { createPublicClient, http, encodeFunctionData, decodeAbiParameters, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { readFileSync } from "node:fs";
import { getBalanceOverride, getAllowanceOverride, ROUTER_ADDRESS } from "hayabusa-router";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const MAIN_BYTECODE_PATH = "/home/claude/defi-toolbox-avalanche/router/contracts/bytecode.hex";
const WOOPP_V2 = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const amountIn = 4950000000n;
const blockNumber = BigInt(80108339) - 1n;
const blockHex = `0x${blockNumber.toString(16)}`;
const DUMMY_SENDER = "0x000000000000000000000000000000000000dEaD";

const bytecode = "0x" + readFileSync(MAIN_BYTECODE_PATH, "utf-8").trim();

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
  args: [[WOOPP_V2 as `0x${string}`], [14], [USDC as `0x${string}`, WAVAX as `0x${string}`], amountIn, ["0x"]],
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
  code: bytecode as Hex,
};

console.log("State override addresses:", Object.keys(stateOverride));
console.log("USDC stateDiff slots:", Object.keys(stateOverride[USDC]?.stateDiff || {}));

// Use debug_traceCall to see what happens
try {
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
    console.log(`${indent}${call.type} ${(call.from||"").slice(0,10)}→${(call.to||"").slice(0,10)} ${(call.input||"").slice(0,20)} out=${(call.output||"").slice(0,34)} ${call.error || ""}`);
    for (const sub of (call.calls || [])) printTrace(sub, depth + 1);
  }
  
  console.log("\nTrace:");
  printTrace(trace);
} catch (e: any) {
  console.log("Trace error:", e.message?.slice(0, 200));
}

// Also try direct eth_call to see if the output is non-0
try {
  const result = await client.request({
    method: "eth_call" as any,
    params: [
      { from: DUMMY_SENDER, to: ROUTER_ADDRESS, data },
      blockHex,
      stateOverride,
    ] as any,
  });
  if (result === "0x" || !result) {
    console.log("eth_call: empty result");
  } else {
    const [out] = decodeAbiParameters([{type:"uint256"}], result as `0x${string}`);
    console.log("eth_call output:", out);
  }
} catch (e: any) {
  console.log("eth_call error:", e.message?.slice(0, 200));
}
