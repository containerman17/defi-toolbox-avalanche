// Check if WooPP V2 works in the main branch's quoteRoute
import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { quoteRoute } from "hayabusa-router";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const WOOPP_V2 = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const amountIn = 4950000000n;
const blockNumber = BigInt(80108339) - 1n;

try {
  const out = await quoteRoute(client, [{
    pool: {
      address: WOOPP_V2,
      providerName: "",
      poolType: 14 as any,
      tokens: [USDC, WAVAX],
      latestSwapBlock: 0,
    },
    tokenIn: USDC,
    tokenOut: WAVAX,
  }], amountIn, blockNumber);
  console.log(`quoteRoute output: ${out}`);
} catch (e: any) {
  console.log(`Error: ${e.message?.slice(0, 300)}`);
}
