import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";
import { quoteRoute } from "hayabusa-router";
import { type StoredPool, type PoolType } from "hayabusa-pools";

const rpcUrl = "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

// Test WOOPP_V2 step from 0x1f84c569
// block 80108339, so blockNumber = 80108338
const blockNumber = 80108338n;

// Test the WooPP V2 step
const woopp_route = [{
  pool: {
    address: "0xaba7ed514217d51630053d73d358ac2502d3f9bb",
    providerName: "woopp_v2",
    poolType: 14 as PoolType,
    tokens: ["0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e", "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7"],
    latestSwapBlock: 0,
  },
  tokenIn: "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e",
  tokenOut: "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7",
}];

try {
  const out = await quoteRoute(client, woopp_route, 4950000000n, blockNumber);
  console.log(`WooPP V2 out: ${out}`);
} catch (e: any) {
  console.log(`WooPP V2 error: ${e.message?.slice(0, 200)}`);
}
