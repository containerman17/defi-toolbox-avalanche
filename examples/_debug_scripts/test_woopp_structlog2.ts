// Use structlog tracer to trace WooPP at opcode level
import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { keccak256, encodeAbiParameters } from "viem";
import { ROUTER_ADDRESS } from "hayabusa-router";
import { readFileSync } from "node:fs";
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

const calldata = encodeSwap([{
  pool: { address: WOOPP, providerName: "", poolType: 14 as any, tokens: [USDC, WAVAX], latestSwapBlock: 0 },
  tokenIn: USDC, tokenOut: WAVAX,
}], amountIn);

// Use structlog but with a JS tracer that captures CALL returns
const jsTracer = `{
  calls: [],
  step: function(log, db) {
    var op = log.op.toString();
    if (op === 'RETURN' || op === 'REVERT') {
      var mem = log.memory;
      var stack = log.stack;
      var offset = stack.peek(0);
      var size = stack.peek(1);
      if (size > 0 && size <= 64) {
        var data = [];
        for (var i = 0; i < Number(size); i++) {
          data.push(mem.getUint8(Number(offset) + i).toString(16).padStart(2, '0'));
        }
        this.calls.push({op: op, depth: log.getDepth(), data: data.join('')});
      }
    }
  },
  result: function(ctx, db) {
    return {calls: this.calls.slice(0, 20), output: '0x' + ctx.output.toString('hex')};
  }
}`;

const trace = await client.request({
  method: "debug_traceCall" as any,
  params: [
    { from: "0x000000000000000000000000000000000000dEaD", to: ROUTER_ADDRESS, data: calldata },
    blockHex,
    { tracer: jsTracer, stateOverrides: stateOverride },
  ] as any,
}) as any;
console.log(JSON.stringify(trace, null, 2));
