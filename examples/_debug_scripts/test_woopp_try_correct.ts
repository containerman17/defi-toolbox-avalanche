// Try calling WooPP with the correct minOutput value (type(uint128).max)
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

// Manually encode WooPP call with correct minOutput = type(uint128).max
const MAX_UINT128 = (1n << 128n) - 1n;

// Build calldata directly matching the real tx format
const calldata_manual = "0xac8bb7d9" +
  ROUTER_ADDRESS.slice(2).toLowerCase().padStart(64, "0") +  // broker
  "0".padStart(64, "0") +  // direction = 0 (sell quote)
  amountIn.toString(16).padStart(64, "0") +  // amount
  MAX_UINT128.toString(16).padStart(64, "0") +  // minOutput = type(uint128).max
  "00000000000000000000000000000000000000000000000000000000000000a0" +  // bytes offset
  "0000000000000000000000000000000000000000000000000000000000000020" +  // bytes length
  WAVAX.slice(2).toLowerCase().padStart(64, "0");  // base token = WAVAX (since selling USDC, base is WAVAX)

console.log("Manual calldata:", calldata_manual.slice(0, 50) + "...");

// Try calling WooPP directly from the router address
const wooppResult = await client.request({
  method: "eth_call" as any,
  params: [
    { from: ROUTER_ADDRESS, to: WOOPP, data: calldata_manual },
    blockHex,
    stateOverride,
  ] as any,
}).catch(e => "err: " + e.message?.slice(0, 200));
console.log("WooPP direct call:", wooppResult);

// Now build a modified Solidity router with correct minOutput
// But we can't recompile... let me try via the router's quoteRoute with a modified approach
// 
// Actually, I should modify the Solidity to use type(uint128).max instead of 0

// For now, let's check: does using type(uint128).max change the outcome?
// Build calldata simulating what _swapWooPPV2 would do with correct minOutput

// The key insight: the current Solidity passes minOutput=0, real tx uses type(uint128).max
// Let me check if WooPP treats these differently

console.log("\ntype(uint128).max:", MAX_UINT128);
