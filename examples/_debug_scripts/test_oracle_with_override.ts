// Check if router code override affects oracle call
import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { readFileSync } from "node:fs";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const ORACLE = "0xd92e3c8f1c5e835e4e76173c4e83bf517f61b737";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const ROUTER = "0x2bef1becdafcfe8990a233d03a98bbb39021c96e";
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

const bytecode = "0x" + readFileSync("/home/claude/defi-toolbox-avalanche/router/contracts/bytecode.hex", "utf-8").trim();

// Call oracle without any override
const result1 = await client.request({
  method: "eth_call" as any,
  params: [{ to: ORACLE, data: "0xc1701b67" + WAVAX.slice(2).padStart(64, "0") }, blockHex] as any,
});
console.log("oracle without override:", result1);

// Call oracle WITH router bytecode override (but oracle not affected)
const result2 = await client.request({
  method: "eth_call" as any,
  params: [
    { to: ORACLE, data: "0xc1701b67" + WAVAX.slice(2).padStart(64, "0") },
    blockHex,
    { [ROUTER]: { code: bytecode as Hex } },
  ] as any,
});
console.log("oracle WITH router code override:", result2);

// Now trace the quoteRoute call to see what oracle returns
const { encodeAbiParameters, keccak256 } = await import("viem");
const { encodeSwap } = await import("../router/encode.ts");
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const amountIn = 4950000000n;
const slot = keccak256(encodeAbiParameters([{type: "address"}, {type: "uint256"}], [ROUTER as Hex, 9n]));

const calldata = encodeSwap([{
  pool: { address: "0xABa7eD514217D51630053d73D358aC2502d3f9BB", providerName: "", poolType: 14 as any, tokens: [USDC, WAVAX], latestSwapBlock: 0 },
  tokenIn: USDC, tokenOut: WAVAX,
}], amountIn);

// Use structs logger to log sload
const trace = await client.request({
  method: "debug_traceCall" as any,
  params: [
    { from: "0x000000000000000000000000000000000000dEaD", to: ROUTER, data: calldata },
    blockHex,
    { 
      tracer: "callTracer",
      stateOverrides: { 
        [ROUTER]: { code: bytecode as Hex },
        [USDC]: { stateDiff: { [slot]: `0x${amountIn.toString(16).padStart(64, "0")}` as Hex } },
      }
    },
  ] as any,
}) as any;

// Find oracle call output
function findOracleCall(call: any): any {
  if (call.to?.toLowerCase() === ORACLE.toLowerCase()) return call;
  for (const sub of (call.calls||[])) {
    const found = findOracleCall(sub);
    if (found) return found;
  }
  return null;
}
const oracleCall = findOracleCall(trace);
console.log("\noracle in quoteRoute trace output:", oracleCall?.output);
