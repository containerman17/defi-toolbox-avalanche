// Debug WooPP V2 via debug_traceCall
import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { quoteRoute } from "hayabusa-router";
import { type StoredPool } from "hayabusa-pools";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

// WooPP V2 step from 0x1f84c569
const step = {
  amountIn: 4950000000n,
  pool: "0xaba7ed514217d51630053d73d358ac2502d3f9bb",
  poolType: 14, // WOOPP_V2
  tokenIn: "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e", // USDC
  tokenOut: "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7", // WAVAX
  blockNumber: BigInt(80108339) - 1n,
};

// Try quoteRoute directly
try {
  const out = await quoteRoute(client, [{
    pool: {
      address: step.pool,
      providerName: "",
      poolType: step.poolType as any,
      tokens: [step.tokenIn, step.tokenOut],
      latestSwapBlock: 0,
    },
    tokenIn: step.tokenIn,
    tokenOut: step.tokenOut,
  }], step.amountIn, step.blockNumber);
  console.log(`quoteRoute output: ${out}`);
} catch (err: any) {
  console.log(`quoteRoute error: ${err.message?.slice(0, 200)}`);
}

// Check WooPP V2 pool state
console.log("\nChecking WooPP V2 state...");

// Check if pool has WAVAX to send
const wavax = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const woopool = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const blockHex = `0x${step.blockNumber.toString(16)}`;

// Get WAVAX balance of WooPP pool
const balanceCall = await client.request({
  method: "eth_call" as any,
  params: [
    { to: wavax, data: "0x70a08231000000000000000000000000aba7ed514217d51630053d73d358ac2502d3f9bb" },
    blockHex,
  ] as any,
});
console.log(`WooPP WAVAX balance: ${BigInt(balanceCall as string)}`);

// Check WooPP pool isPaused
const pauseCall = await client.request({
  method: "eth_call" as any,
  params: [
    { to: woopool, data: "0xb187bd26" }, // isPaused()
    blockHex,
  ] as any,
});
console.log(`WooPP isPaused result (raw): ${pauseCall}`);

