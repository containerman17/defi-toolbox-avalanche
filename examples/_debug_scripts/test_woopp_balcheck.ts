// Check WAVAX balance of WooPP and router after quoteRoute
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
const stateOverride = {
  [ROUTER_ADDRESS]: { code: bytecode as Hex },
  [USDC]: { stateDiff: { [slot]: `0x${amountIn.toString(16).padStart(64, "0")}` as Hex } },
};

// Check WooPP WAVAX balance before
const wooppWavaxBefore = await client.request({
  method: "eth_call" as any,
  params: [{ to: WAVAX, data: "0x70a08231" + WOOPP.slice(2).toLowerCase().padStart(64, "0") }, blockHex, stateOverride] as any,
});
const routerWavaxBefore = await client.request({
  method: "eth_call" as any,
  params: [{ to: WAVAX, data: "0x70a08231" + ROUTER_ADDRESS.slice(2).toLowerCase().padStart(64, "0") }, blockHex, stateOverride] as any,
});
console.log("WooPP WAVAX before:", BigInt(wooppWavaxBefore as string));
console.log("Router WAVAX before:", BigInt(routerWavaxBefore as string));

// Now trace eth_call at opcode level using debug_traceCall with a MINIMAL tracer
// that captures SSTORE values
const jsTracer2 = `{
  sstores: [],
  step: function(log) {
    var op = log.op.toString();
    if (op === 'SSTORE') {
      var key = log.stack.peek(0).toString(16);
      var val = log.stack.peek(1).toString(16);
      this.sstores.push({depth: log.getDepth(), key: key, val: val.slice(-20)});
    }
  },
  result: function(ctx) {
    return this.sstores;
  }
}`;

const trace = await client.request({
  method: "debug_traceCall" as any,
  params: [
    { from: "0x000000000000000000000000000000000000dEaD", to: ROUTER_ADDRESS, data: encodeSwap([{
      pool: { address: WOOPP, providerName: "", poolType: 14 as any, tokens: [USDC, WAVAX], latestSwapBlock: 0 },
      tokenIn: USDC, tokenOut: WAVAX,
    }], amountIn) },
    blockHex,
    { tracer: jsTracer2, stateOverrides: stateOverride },
  ] as any,
}).catch(e => "err: " + e.message?.slice(0, 100));
console.log("\nSSTORE ops:", trace);
