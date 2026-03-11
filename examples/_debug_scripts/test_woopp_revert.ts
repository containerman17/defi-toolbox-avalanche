// Try calling WooPP with direction=1 to see if it reverts differently
import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { readFileSync } from "node:fs";
import { keccak256, encodeAbiParameters } from "viem";
import { ROUTER_ADDRESS } from "hayabusa-router";

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

// Try WooPP swap with direction=1 (isSellingBase=true) - should this be WAVAX→USDC?
// In real tx for this split step, direction=0 means USDC→WAVAX (selling USDC, buying WAVAX)
// But our Solidity says: isSellingBase = (tokenIn != USDC) = false, so direction=0

// What if we need to pass isSellingBase=true? Let me try direction=1
// This would encode: base token = WAVAX (since tokenIn=USDC, isSellingBase=false, baseToken=tokenOut=WAVAX)
const USDC_LOWER = USDC.toLowerCase();
const WAVAX_LOWER = WAVAX.toLowerCase();

// Try direction=1 (our Solidity: direction=0 for USDC→WAVAX, but maybe it should be 1?)
const MAX_UINT128 = (1n << 128n) - 1n;

// Build calldata with direction=1 and base token = USDC (since we're "selling base")
const calldata_dir1 = 
  "0xac8bb7d9" +
  ROUTER_ADDRESS.slice(2).toLowerCase().padStart(64, "0") +  // broker
  "1".padStart(64, "0") +  // direction = 1 (sell base)
  amountIn.toString(16).padStart(64, "0") +  // amount (base amount)
  MAX_UINT128.toString(16).padStart(64, "0") +  // minOutput = max (like real tx)
  "00000000000000000000000000000000000000000000000000000000000000a0" +
  "0000000000000000000000000000000000000000000000000000000000000020" +
  USDC.slice(2).toLowerCase().padStart(64, "0");  // base token = USDC (since selling base=USDC)

console.log("Trying direction=1, base=USDC (selling USDC for WAVAX?)");
const result_dir1 = await client.request({
  method: "eth_call" as any,
  params: [
    { from: ROUTER_ADDRESS, to: WOOPP, data: calldata_dir1 },
    blockHex,
    stateOverride,
  ] as any,
}).catch(e => "err: " + e.message?.slice(0, 200));
console.log("Result dir=1:", result_dir1);

// Try with base token = WAVAX (direction=0, selling USDC=quote, base is WAVAX)
const calldata_dir0 = 
  "0xac8bb7d9" +
  ROUTER_ADDRESS.slice(2).toLowerCase().padStart(64, "0") +
  "0".padStart(64, "0") +
  amountIn.toString(16).padStart(64, "0") +
  MAX_UINT128.toString(16).padStart(64, "0") +
  "00000000000000000000000000000000000000000000000000000000000000a0" +
  "0000000000000000000000000000000000000000000000000000000000000020" +
  WAVAX.slice(2).toLowerCase().padStart(64, "0");  // base token = WAVAX

console.log("\nTrying direction=0, base=WAVAX, minOutput=max (like real tx)");
const result_dir0_max = await client.request({
  method: "eth_call" as any,
  params: [
    { from: ROUTER_ADDRESS, to: WOOPP, data: calldata_dir0 },
    blockHex,
    stateOverride,
  ] as any,
}).catch(e => "err: " + e.message?.slice(0, 200));
console.log("Result dir=0, min=max:", result_dir0_max);

// What about direction=0, minOutput=0, base=WAVAX (our current encoding)?
const calldata_ours = 
  "0xac8bb7d9" +
  ROUTER_ADDRESS.slice(2).toLowerCase().padStart(64, "0") +
  "0".padStart(64, "0") +
  amountIn.toString(16).padStart(64, "0") +
  "0".padStart(64, "0") +  // minOutput = 0
  "00000000000000000000000000000000000000000000000000000000000000a0" +
  "0000000000000000000000000000000000000000000000000000000000000020" +
  WAVAX.slice(2).toLowerCase().padStart(64, "0");

console.log("\nTrying direction=0, base=WAVAX, minOutput=0 (our current)");
const result_ours = await client.request({
  method: "eth_call" as any,
  params: [
    { from: ROUTER_ADDRESS, to: WOOPP, data: calldata_ours },
    blockHex,
    stateOverride,
  ] as any,
}).catch(e => "err: " + e.message?.slice(0, 200));
console.log("Result (ours):", result_ours);
