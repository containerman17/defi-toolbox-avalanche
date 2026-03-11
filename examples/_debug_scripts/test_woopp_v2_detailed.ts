// Detailed test of WooPP V2 after fix
import { createPublicClient, http, decodeAbiParameters, encodeFunctionData, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { readFileSync } from "node:fs";
import { getBalanceOverride, getAllowanceOverride, ROUTER_ADDRESS } from "hayabusa-router";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });
const MAIN_BYTECODE = "0x" + readFileSync("/home/claude/defi-toolbox-avalanche/router/contracts/bytecode.hex", "utf-8").trim();

const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const amountIn = 4950000000n;
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;
const DUMMY_SENDER = "0x000000000000000000000000000000000000dEaD";

// What will the WooPP calldata be?
// direction=0, tokenIn=USDC, data=USDC (tokenIn)
const expectedCalldata = "0xac8bb7d9" +
  DUMMY_SENDER.slice(2).padStart(64, "0") + // broker = router address
  "0000000000000000000000000000000000000000000000000000000000000000" + // direction=0
  amountIn.toString(16).padStart(64, "0") + // amount
  "0000000000000000000000000000000000000000000000000000000000000000" + // minOutput
  "00000000000000000000000000000000000000000000000000000000000000a0" + // bytes offset
  "0000000000000000000000000000000000000000000000000000000000000020" + // bytes length
  USDC.slice(2).padStart(64, "0"); // data = USDC (tokenIn)

// Real calldata was similar but with LFJ aggregator as broker, not router
console.log("Expected WooPP calldata (first 200 chars):", expectedCalldata.slice(0, 200));

// Build proper state override
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
stateOverride[ROUTER_ADDRESS] = { code: MAIN_BYTECODE as Hex };

const swapAbi = [{
  name: "swap",
  type: "function",
  inputs: [
    { name: "pools", type: "address[]" },
    { name: "poolTypes", type: "uint8[]" },
    { name: "tokens", type: "address[]" },
    { name: "amountIn", type: "uint256" },
    { name: "extraDatas", type: "bytes[]" },
  ],
  outputs: [{ type: "uint256" }],
}] as const;

const data = encodeFunctionData({
  abi: swapAbi,
  functionName: "swap",
  args: [[WOOPP as `0x${string}`], [14], [USDC as `0x${string}`, WAVAX as `0x${string}`], amountIn, ["0x"]],
});

// Try eth_call
try {
  const result = await client.request({
    method: "eth_call" as any,
    params: [{ from: DUMMY_SENDER, to: ROUTER_ADDRESS, data }, blockHex, stateOverride] as any,
  });
  const [out] = decodeAbiParameters([{type:"uint256"}], result as `0x${string}`);
  console.log("eth_call output:", out);
} catch (e: any) {
  console.log("eth_call error:", e.message?.slice(0, 200));
}

// Also try calling WooPP directly with the correct calldata (using ROUTER as broker)
const routerBrokerCalldata = "0xac8bb7d9" +
  ROUTER_ADDRESS.slice(2).padStart(64, "0") + // broker = router
  "0000000000000000000000000000000000000000000000000000000000000000" + // direction=0  
  amountIn.toString(16).padStart(64, "0") +
  "0000000000000000000000000000000000000000000000000000000000000000" +
  "00000000000000000000000000000000000000000000000000000000000000a0" +
  "0000000000000000000000000000000000000000000000000000000000000020" +
  USDC.slice(2).padStart(64, "0"); // data = USDC (tokenIn)

// Try WooPP V2 directly (this will fail since we don't have USDC)
try {
  const result = await client.request({
    method: "eth_call" as any,
    params: [{ from: ROUTER_ADDRESS, to: WOOPP, data: routerBrokerCalldata }, blockHex] as any,
  });
  console.log("Direct WooPP call (no state override):", result?.slice(0, 66));
} catch (e: any) {
  // Expected: need USDC balance
  console.log("Direct WooPP call error (expected):", e.message?.slice(0, 100));
}
