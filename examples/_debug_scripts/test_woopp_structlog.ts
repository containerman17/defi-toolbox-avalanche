// Use structlog tracer to see opcode-level execution of WooPP quoteRoute
import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { readFileSync } from "node:fs";
import { keccak256, encodeAbiParameters } from "viem";
import { ROUTER_ADDRESS } from "hayabusa-router";
import { encodeSwap } from "../router/encode.ts";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const WOOPP = "0xABa7eD514217D51630053d73D358aC2502d3f9BB";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const amountIn = 4950000000n;
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

const bytecode = "0x" + readFileSync("/home/claude/defi-toolbox-avalanche/router/contracts/bytecode.hex", "utf-8").trim();
const slot = keccak256(encodeAbiParameters([{type: "address"}, {type: "uint256"}], [ROUTER_ADDRESS as Hex, 9n]));

const calldata = encodeSwap([{
  pool: { address: WOOPP, providerName: "", poolType: 14 as any, tokens: [USDC, WAVAX], latestSwapBlock: 0 },
  tokenIn: USDC, tokenOut: WAVAX,
}], amountIn);

const stateOverride = {
  [ROUTER_ADDRESS]: { code: bytecode as Hex },
  [USDC]: { stateDiff: { [slot]: `0x${amountIn.toString(16).padStart(64, "0")}` as Hex } },
};

// Use a JS tracer that captures CALL results
const jsTracer = `{
  calls: [],
  call: function(log, db) {
    this.calls.push({
      depth: log.getDepth(),
      op: log.op.toString(),
      to: '0x' + log.contract.getAddress().toString('hex'),
      val: log.stack.peek(0).toString(16),
    });
  },
  result: function(ctx, db) {
    return {output: '0x' + ctx.output.toString('hex'), error: ctx.error};
  }
}`;

// Just use callTracer on the eth_call (which uses correct state)
// Let me use a different approach - intercept at the CALL level
// Actually let me just try to see the return value from WooPP's swap function

// Create a custom router that logs the WooPP output
// Actually, let me just check: what does WooPP return as output?

// Use the prestate+callTracer approach
const trace2 = await client.request({
  method: "debug_traceCall" as any,
  params: [
    { from: "0x000000000000000000000000000000000000dEaD", to: ROUTER_ADDRESS, data: calldata },
    blockHex,
    { 
      tracer: "callTracer",
      stateOverrides: stateOverride,
    },
  ] as any,
}) as any;

function findWooPPCall(call: any): any {
  if (call.to?.toLowerCase() === WOOPP.toLowerCase()) return call;
  for (const sub of (call.calls||[])) {
    const found = findWooPPCall(sub);
    if (found) return found;
  }
  return null;
}

const wooppCall = findWooPPCall(trace2);
console.log("WooPP call output:", wooppCall?.output);
console.log("WooPP call error:", wooppCall?.error);

// Now use debug_traceCall with eth_call stateOverride semantics by checking 
// what value the oracle returned when called INSIDE the ROUTER's eth_call context

// Actually, let me try a completely different approach:
// Use eth_call with a custom tracer (geth supports it in some versions)

// Instead, let's just verify by calling the oracle inside our router's eth_call:
// Manually encode a call that: 
// 1. Checks oracle at block
// 2. Returns the oracle output
// This tells us if the oracle has the right data in eth_call context

// Simplest: just call oracle directly with same block+stateOverrides
const oracleResult = await client.request({
  method: "eth_call" as any,
  params: [
    { to: "0xd92e3c8f1c5e835e4e76173c4e83bf517f61b737", data: "0xc1701b67" + WAVAX.slice(2).padStart(64, "0") },
    blockHex,
    stateOverride,
  ] as any,
});
console.log("\noracle result in eth_call with router override:", oracleResult);
// If oracle returns valid data, then WooPP should calculate correct output

// Let me check WAVAX.balanceOf(WooPP) with our override
const wavaxBal = await client.request({
  method: "eth_call" as any,
  params: [
    { to: WAVAX, data: "0x70a08231" + WOOPP.slice(2).toLowerCase().padStart(64, "0") },
    blockHex,
    stateOverride,
  ] as any,
});
console.log("WooPP WAVAX balance in eth_call with override:", BigInt(wavaxBal as string));
