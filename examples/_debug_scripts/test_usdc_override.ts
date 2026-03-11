// Verify USDC state override works correctly
import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { getBalanceOverride, getAllowanceOverride, ROUTER_ADDRESS } from "hayabusa-router";
import { readFileSync } from "node:fs";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });
const BYTECODE = "0x" + readFileSync("/home/claude/defi-toolbox-avalanche/router/contracts/bytecode.hex", "utf-8").trim();

const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const DUMMY_SENDER = "0x000000000000000000000000000000000000dEaD";
const amountIn = 4950000000n;
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

const balOverride = getBalanceOverride(USDC, amountIn, DUMMY_SENDER);
const allowOverride = getAllowanceOverride(USDC, DUMMY_SENDER, ROUTER_ADDRESS);

console.log("USDC balance override slots:", JSON.stringify(balOverride, null, 2));
console.log("\nUSDC allowance override slots:", JSON.stringify(allowOverride, null, 2));

// Build state override
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
stateOverride[ROUTER_ADDRESS] = { code: BYTECODE as Hex };

// Now check DUMMY_SENDER USDC balance with and without state override
const balWithoutOverride = await client.request({
  method: "eth_call" as any,
  params: [{ to: USDC, data: "0x70a08231" + DUMMY_SENDER.slice(2).padStart(64, "0") }, blockHex] as any,
});
console.log("\nDUMMY_SENDER USDC balance WITHOUT override:", BigInt(balWithoutOverride as string));

const balWithOverride = await client.request({
  method: "eth_call" as any,
  params: [{ to: USDC, data: "0x70a08231" + DUMMY_SENDER.slice(2).padStart(64, "0") }, blockHex, stateOverride] as any,
});
console.log("DUMMY_SENDER USDC balance WITH override:", BigInt(balWithOverride as string));

// Also test from ROUTER_ADDRESS's perspective
const balRouterWithout = await client.request({
  method: "eth_call" as any,
  params: [{ to: USDC, data: "0x70a08231" + ROUTER_ADDRESS.slice(2).padStart(64, "0") }, blockHex] as any,
});
console.log("\nROUTER USDC balance WITHOUT override:", BigInt(balRouterWithout as string));
