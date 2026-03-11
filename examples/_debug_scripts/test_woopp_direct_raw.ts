// Call WooPP V2 directly to see what it returns
import { createPublicClient, http, encodeAbiParameters, decodeAbiParameters, type Hex } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const WOOPP = "0xABa7eD514217D51630053d73D358aC2502d3f9BB";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const LFJ_AGGREGATOR = "0x63242a4ea82847b20e506b63b0e2e2eff0cc6cb0";
const blockHex = `0x${(80108339n - 1n).toString(16)}`;

// Build swap calldata for USDC -> WAVAX (sell quote, direction=0, baseToken=WAVAX)
function buildWooPPCalldata(broker: string, isSellingBase: boolean, amountIn: bigint, baseToken: string): string {
  const direction = isSellingBase ? 1n : 0n;
  const args = encodeAbiParameters(
    [{type:"address"},{type:"uint256"},{type:"uint256"},{type:"uint256"}],
    [broker as `0x${string}`, direction, amountIn, 0n]
  );
  const dataOffset = "00000000000000000000000000000000000000000000000000000000000000a0";
  const dataLength = "0000000000000000000000000000000000000000000000000000000000000020";
  const baseTokenPadded = baseToken.slice(2).padStart(64, "0");
  return "0xac8bb7d9" + args.slice(2) + dataOffset + dataLength + baseTokenPadded;
}

// Try calling the query function instead (no state change)
// querySwap(address fromToken, address toToken, uint256 fromAmount) or similar
// Let's check what functions exist by calling known selectors

// The sell path: USDC -> WAVAX, direction=0, broker=LFJ_AGGREGATOR
const calldata = buildWooPPCalldata(LFJ_AGGREGATOR, false, 4950000000n, WAVAX);
console.log("Calldata:", calldata.slice(0, 100));

// Try static call - it won't work since it's a state-changing function
// But let's try eth_call anyway to see what happens
try {
  const result = await client.request({
    method: "eth_call" as any,
    params: [
      { from: LFJ_AGGREGATOR, to: WOOPP, data: calldata },
      blockHex,
    ] as any,
  });
  console.log("Result:", result);
} catch (e: any) {
  console.log("Expected revert:", e.message?.slice(0, 150));
}

// Let's check if WooPP has a query/view function for getting the swap amount
// Common selectors: tryQuery, querySwap, getSwapInfo, getPrice
const viewSelectors = [
  { name: "tryQuery(address,address,uint256)", data: "0x74ba59c7" },
  { name: "query(address,address,uint256)", data: "0x" },
];

// Build query calldata: tryQuery(USDC, WAVAX, 4950000000)  
const queryCalldata = "0x74ba59c7" + 
  USDC.slice(2).padStart(64, "0") + 
  WAVAX.slice(2).padStart(64, "0") + 
  (4950000000n).toString(16).padStart(64, "0");

try {
  const result = await client.request({
    method: "eth_call" as any,
    params: [
      { from: LFJ_AGGREGATOR, to: WOOPP, data: queryCalldata },
      blockHex,
    ] as any,
  });
  const decoded = decodeAbiParameters([{type:"uint256"}], result as `0x${string}`);
  console.log("tryQuery USDC->WAVAX result:", decoded[0]);
} catch (e: any) {
  console.log("tryQuery failed:", e.message?.slice(0, 150));
}

// Also try with swapped tokens
const queryCalldata2 = "0x74ba59c7" + 
  WAVAX.slice(2).padStart(64, "0") + 
  USDC.slice(2).padStart(64, "0") + 
  (4950000000n).toString(16).padStart(64, "0");

try {
  const result = await client.request({
    method: "eth_call" as any,
    params: [
      { from: LFJ_AGGREGATOR, to: WOOPP, data: queryCalldata2 },
      blockHex,
    ] as any,
  });
  const decoded = decodeAbiParameters([{type:"uint256"}], result as `0x${string}`);
  console.log("tryQuery WAVAX->USDC result:", decoded[0]);
} catch (e: any) {
  console.log("tryQuery WAVAX->USDC failed:", e.message?.slice(0, 150));
}
