import { createPublicClient, http, webSocket, encodeAbiParameters, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { readFileSync } from "fs";
import { join } from "path";
import { encodeSwap } from "../router/encode.ts";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });
const ROUTER_ADDRESS = "0x2bef1becdafcfe8990a233d03a98bbb39021c96e";
const DUMMY_SENDER = "0x000000000000000000000000000000000000dEaD";

function getRouterBytecode(): Hex {
  const hex = readFileSync(join(import.meta.dirname!, "../router/contracts/bytecode.hex"), "utf-8").trim();
  return `0x${hex}` as Hex;
}

// Trident route: WooPP(USDt→WAVAX) → Trident(WAVAX→USDC)
const pool = "0x895114d100b5013b700f03900f825625d7db35cc";
const bentoBox = "0x0711b6026068f736bae6b213031fce978d48e026";
const wavax = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const usdc = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const usdt = "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7";
const blockNumber = 80114945n;

// First test just the Trident hop alone (WAVAX→USDC)
console.log("=== Test Trident standalone ===");
const route = [
  {
    pool: { address: pool, providerName: "", poolType: 20 as const, tokens: [wavax, usdc], latestSwapBlock: 0, extraData: `bento=${bentoBox}` },
    tokenIn: wavax,
    tokenOut: usdc,
  },
];
const amountIn = 524229608603175n; // ~0.0005 WAVAX

const calldata = encodeSwap(route, amountIn);
console.log("calldata length:", calldata.length);

// Use debug_traceCall to see why it reverts
const stateOverride: Record<string, any> = {};

// Set WAVAX balance on router
import { getBalanceOverride } from "../router/overrides.ts";
const balOvr = getBalanceOverride(wavax, amountIn, ROUTER_ADDRESS);
for (const [addr, val] of Object.entries(balOvr)) {
  stateOverride[addr] = { stateDiff: (val as any).stateDiff };
}
stateOverride[ROUTER_ADDRESS] = {
  ...(stateOverride[ROUTER_ADDRESS] ?? {}),
  code: getRouterBytecode(),
};

try {
  const result = await client.request({
    method: "debug_traceCall" as any,
    params: [
      { from: DUMMY_SENDER, to: ROUTER_ADDRESS, data: calldata, gas: "0x5F5E100" },
      `0x${blockNumber.toString(16)}`,
      { tracer: "callTracer", tracerConfig: { onlyTopCall: false, withLog: true }, stateOverrides: stateOverride },
    ] as any,
  });
  const r = result as any;
  console.log("gas:", r.gas, "failed:", r.failed);
  console.log("error:", r.error);
  console.log("revertReason:", r.revertReason);
  // Print call tree
  function printCalls(call: any, depth = 0) {
    const indent = "  ".repeat(depth);
    const to = call.to?.slice(0, 10) || "?";
    const input = call.input?.slice(0, 10) || "?";
    console.log(`${indent}${call.type} ${to} ${input} gas=${call.gas} gasUsed=${call.gasUsed} err=${call.error || ""}`);
    if (call.calls) for (const c of call.calls) printCalls(c, depth + 1);
  }
  printCalls(r);
} catch (e: any) {
  console.error("trace error:", e.message?.slice(0, 300));
}

process.exit(0);
