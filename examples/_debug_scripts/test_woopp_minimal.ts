// Minimal test: call WooPP V2 swap directly from scratch
import { createPublicClient, http, decodeAbiParameters, encodeFunctionData, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { readFileSync } from "node:fs";
import { keccak256, encodeAbiParameters, pad, toHex, maxUint256 } from "viem";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const BYTECODE = "0x" + readFileSync("/home/claude/defi-toolbox-avalanche/router/contracts/bytecode.hex", "utf-8").trim();
const ROUTER = "0x2bef1becdafcfe8990a233d03a98bbb39021c96e";
const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const DUMMY = "0x000000000000000000000000000000000000dEaD";
const amountIn = 4950000000n;
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

// Compute USDC balance slot for DUMMY (USDC uses slot 9 mapping)
function mapSlot(key: string, slot: number): Hex {
  return keccak256(encodeAbiParameters(
    [{type: "address"}, {type: "uint256"}],
    [key as `0x${string}`, BigInt(slot)]
  ));
}

// USDC: slot 9 = balances, slot 10 = allowances  
const balSlot = mapSlot(DUMMY, 9);
const allowSlot = keccak256(encodeAbiParameters(
  [{type: "address"}, {type: "bytes32"}],
  [ROUTER as `0x${string}`, mapSlot(DUMMY, 10)]
));

console.log("Balance slot:", balSlot);
console.log("Allowance slot:", allowSlot);

// Verify balance slot works
const stateOverride: Record<string, any> = {
  [USDC]: {
    stateDiff: {
      [balSlot]: pad(toHex(amountIn), {size: 32}) as Hex,
      [allowSlot]: pad(toHex(maxUint256), {size: 32}) as Hex,
    }
  },
  [ROUTER]: {
    code: BYTECODE as Hex,
  }
};

// Check balance
const balCall = await client.request({
  method: "eth_call" as any,
  params: [{ to: USDC, data: "0x70a08231" + DUMMY.slice(2).padStart(64, "0") }, blockHex, stateOverride] as any,
});
const [bal] = decodeAbiParameters([{type:"uint256"}], balCall as `0x${string}`);
console.log("DUMMY USDC balance (should be 4950000000):", bal);

// Now test swap via router
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

const swapData = encodeFunctionData({
  abi: swapAbi,
  functionName: "swap",
  args: [[WOOPP as `0x${string}`], [14], [USDC as `0x${string}`, WAVAX as `0x${string}`], amountIn, ["0x"]],
});

try {
  const result = await client.request({
    method: "eth_call" as any,
    params: [{ from: DUMMY, to: ROUTER, data: swapData }, blockHex, stateOverride] as any,
  });
  const [out] = decodeAbiParameters([{type:"uint256"}], result as `0x${string}`);
  console.log("Swap output:", out);
} catch (e: any) {
  console.log("Swap error:", e.message?.slice(0, 200));
}
