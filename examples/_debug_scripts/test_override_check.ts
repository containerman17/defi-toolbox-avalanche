// Check if USDC state override works correctly
import { createPublicClient, http, decodeAbiParameters, encodeFunctionData, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { getBalanceOverride, getAllowanceOverride, ROUTER_ADDRESS } from "hayabusa-router";
import { readFileSync } from "node:fs";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const DUMMY_SENDER = "0x000000000000000000000000000000000000dEaD";
const amountIn = 4950000000n;
const blockHex = "0x4C65B32"; // block 80108338

const bytecode = "0x" + readFileSync("/home/claude/defi-toolbox-avalanche/router/contracts/bytecode.hex", "utf-8").trim();

// Build state override
const balOverride = getBalanceOverride(USDC, amountIn, DUMMY_SENDER);
const allowOverride = getAllowanceOverride(USDC, DUMMY_SENDER, ROUTER_ADDRESS);

const merged: Record<string, Record<string, Hex>> = {};
for (const ovr of [balOverride, allowOverride]) {
  for (const [addr, val] of Object.entries(ovr)) {
    if (!merged[addr]) merged[addr] = {};
    Object.assign(merged[addr], val.stateDiff);
  }
}

const stateOverride: Record<string, any> = {};
for (const [address, slots] of Object.entries(merged)) {
  stateOverride[address] = { stateDiff: slots };
}
stateOverride[ROUTER_ADDRESS] = { code: bytecode as Hex };

console.log("USDC override slots:", JSON.stringify(stateOverride[USDC], null, 2));

// Call USDC.balanceOf(DUMMY_SENDER) with state override
const balanceOfAbi = [{
  name: "balanceOf",
  type: "function",
  inputs: [{ name: "account", type: "address" }],
  outputs: [{ type: "uint256" }],
}] as const;

const balanceOfData = encodeFunctionData({
  abi: balanceOfAbi,
  functionName: "balanceOf",
  args: [DUMMY_SENDER as `0x${string}`],
});

try {
  const result = await client.request({
    method: "eth_call" as any,
    params: [
      { to: USDC, data: balanceOfData },
      blockHex,
      stateOverride,
    ] as any,
  });
  const [bal] = decodeAbiParameters([{type:"uint256"}], result as `0x${string}`);
  console.log(`USDC.balanceOf(DUMMY_SENDER): ${bal}`);
} catch (e: any) {
  console.log("Error:", e.message?.slice(0, 150));
}
