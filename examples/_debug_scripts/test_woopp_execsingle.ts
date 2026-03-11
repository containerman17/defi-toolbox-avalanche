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

// Try quoteMulti to get a quote for a single pool
// quoteMulti(address pool, uint8 poolType, address tokenIn, address tokenOut, uint256[] amounts, bytes extraData)
const quoteMultiCalldata = 
  "0x3a87c3fa" + // quoteMulti selector (let me compute)
  "";
  
// Compute quoteMulti selector
const { toBytes } = await import("viem");
const quoteMultiSel = keccak256(toBytes("quoteMulti(address,uint8,address,address,uint256[],bytes)")).slice(0, 10);
console.log("quoteMulti selector:", quoteMultiSel);

// Build quoteMulti calldata
const calldata = 
  quoteMultiSel.slice(2) +
  WOOPP.slice(2).padStart(64, "0") +  // pool
  "000000000000000000000000000000000000000000000000000000000000000e" +  // poolType = 14
  USDC.slice(2).padStart(64, "0") +  // tokenIn
  WAVAX.slice(2).padStart(64, "0") +  // tokenOut
  "00000000000000000000000000000000000000000000000000000000000000c0" +  // amounts offset
  "0000000000000000000000000000000000000000000000000000000000000100" +  // extraData offset  
  "0000000000000000000000000000000000000000000000000000000000000001" +  // amounts.length = 1
  amountIn.toString(16).padStart(64, "0") +  // amounts[0]
  "0000000000000000000000000000000000000000000000000000000000000000";  // extraData length = 0

const result = await client.request({
  method: "eth_call" as any,
  params: [
    { from: "0x000000000000000000000000000000000000dEaD", to: ROUTER_ADDRESS, data: "0x" + calldata },
    blockHex,
    stateOverride,
  ] as any,
}).catch(e => "err: " + e.message?.slice(0, 300));
console.log("quoteMulti result:", result);
