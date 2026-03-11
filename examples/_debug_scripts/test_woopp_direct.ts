import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { readFileSync } from "node:fs";
import { ROUTER_ADDRESS } from "hayabusa-router";
import { encodeSwap } from "../router/encode.ts";
import { keccak256, encodeAbiParameters } from "viem";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const WOOPP = "0xABa7eD514217D51630053d73D358aC2502d3f9BB";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const amountIn = 4950000000n;
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

const bytecode = "0x" + readFileSync("/home/claude/defi-toolbox-avalanche/router/contracts/bytecode.hex", "utf-8").trim();

const route = [{
  pool: { address: WOOPP, providerName: "", poolType: 14 as any, tokens: [USDC, WAVAX], latestSwapBlock: 0 },
  tokenIn: USDC, tokenOut: WAVAX,
}];
const calldata = encodeSwap(route, amountIn);

// Build state overrides
const slot = keccak256(encodeAbiParameters([{type: "address"}, {type: "uint256"}], [ROUTER_ADDRESS as Hex, 9n]));
const stateOverride = {
  [ROUTER_ADDRESS]: { code: bytecode as Hex },
  [USDC]: { stateDiff: { [slot]: `0x${amountIn.toString(16).padStart(64, "0")}` as Hex } },
};

// Call via eth_call
const result = await client.request({
  method: "eth_call" as any,
  params: [
    { from: "0x000000000000000000000000000000000000dEaD", to: ROUTER_ADDRESS, data: calldata },
    blockHex,
    stateOverride,
  ] as any,
}).catch(e => `err: ${e.message?.slice(0, 200)}`);
console.log("eth_call result:", result);

// Try without state overrides (use the deployed router bytecode)
const result2 = await client.request({
  method: "eth_call" as any,
  params: [
    { from: "0x000000000000000000000000000000000000dEaD", to: ROUTER_ADDRESS, data: calldata },
    blockHex,
  ] as any,
}).catch(e => `err: ${e.message?.slice(0, 200)}`);
console.log("eth_call without overrides:", result2);
