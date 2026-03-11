// Test WooPP V2 USDC->WAVAX with small amountIn (matching 80da4993)
import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";
import { quoteRoute } from "hayabusa-router";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const WOOPP_V2 = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";

// Small amount from 0x80da4993
const amountInSmall = 101747500n;
// Large amount from 0x1f84c569
const amountInLarge = 4950000000n;

const blockSmall = BigInt(80100897) - 1n; // 80da4993 block
const blockLarge = BigInt(80108339) - 1n; // 1f84c569 block

for (const [label, amount, block] of [
  ["80da4993 (101747500)", amountInSmall, blockSmall],
  ["1f84c569 (4950000000)", amountInLarge, blockLarge],
]) {
  try {
    const out = await quoteRoute(client, [{
      pool: { address: WOOPP_V2, providerName: "", poolType: 14 as any, tokens: [USDC, WAVAX], latestSwapBlock: 0 },
      tokenIn: USDC, tokenOut: WAVAX,
    }], amount as bigint, block as bigint);
    console.log(`${label}: ${out}`);
  } catch (e: any) {
    console.log(`${label}: error - ${e.message?.slice(0,100)}`);
  }
}
