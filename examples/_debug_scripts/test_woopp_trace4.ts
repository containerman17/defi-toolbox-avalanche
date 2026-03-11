// Trace WooPP V2 via eth_call (not debug_traceCall) to see actual behavior
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

// Check USDC balance slot for ROUTER in USDC's storage
console.log("USDC balance slot for ROUTER:", slot);

// Check if slot is correct by reading USDC balance of router
const routerUSDCBal = await client.request({
  method: "eth_call" as any,
  params: [
    { to: USDC, data: "0x70a08231" + ROUTER_ADDRESS.slice(2).padStart(64, "0") },
    blockHex,
  ] as any,
});
console.log("Router USDC balance at block:", BigInt(routerUSDCBal as string));

const slotVal = await client.getStorageAt({ address: USDC as `0x${string}`, slot: slot as `0x${string}`, blockNumber: block });
console.log("USDC slot value at block:", slotVal);

// Now set the slot and verify it works
const stateOverride = {
  [ROUTER_ADDRESS]: { code: bytecode as Hex },
  [USDC]: { stateDiff: { [slot]: `0x${amountIn.toString(16).padStart(64, "0")}` as Hex } },
};

// Verify router balance with override
const routerBalWithOverride = await client.request({
  method: "eth_call" as any,
  params: [
    { to: USDC, data: "0x70a08231" + ROUTER_ADDRESS.slice(2).padStart(64, "0") },
    blockHex,
    stateOverride,
  ] as any,
});
console.log("Router USDC balance with override:", BigInt(routerBalWithOverride as string));

// Full call
const calldata = encodeSwap([{
  pool: { address: WOOPP, providerName: "", poolType: 14 as any, tokens: [USDC, WAVAX], latestSwapBlock: 0 },
  tokenIn: USDC, tokenOut: WAVAX,
}], amountIn);

const result = await client.request({
  method: "eth_call" as any,
  params: [
    { from: "0x000000000000000000000000000000000000dEaD", to: ROUTER_ADDRESS, data: calldata },
    blockHex,
    stateOverride,
  ] as any,
}).catch(e => "err: " + e.message?.slice(0, 200));
console.log("\nquoteRoute result:", result);

// Let's also check what WOOPP balance of WAVAX is
const wooppWAVAXBal = await client.request({
  method: "eth_call" as any,
  params: [
    { to: WAVAX, data: "0x70a08231" + "000000000000000000000000aba7ed514217d51630053d73d358ac2502d3f9bb" },
    blockHex,
    stateOverride,
  ] as any,
});
console.log("WooPP WAVAX balance:", BigInt(wooppWAVAXBal as string));

// Let's also check WOOPP's USDC balance  
const wooppUSDCBal = await client.request({
  method: "eth_call" as any,
  params: [
    { to: USDC, data: "0x70a08231" + "000000000000000000000000aba7ed514217d51630053d73d358ac2502d3f9bb" },
    blockHex,
    stateOverride,
  ] as any,
});
console.log("WooPP USDC balance:", BigInt(wooppUSDCBal as string));
