import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";
import { quoteRoute } from "../../router/quote.ts";
import { loadPools } from "hayabusa-pools";

const RPC_URL = process.env.RPC_URL || "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(RPC_URL) });

const BLOCK = 80109190n;
const LINK = "0x5947bb275c521040051d82396192181b413227a3";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const USDC_E = "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664";
const USDt = "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7";

const POOL_ALG = "0x177a7376860a04eea6bf7fb3f39bf10c49d3b42c";   // algebra LINK→USDC
const POOL_V3_USDC_USDCE = "0x01c7c6066ec10b1cd4821e13b9fb063680ffa083"; // uniswap_v3 USDC→USDC.e  
const POOL_PLAT = "0x5ee9008e49b922cafef9dde21446934547e42ad6"; // platypus USDC.e→USDt
const POOL_V4 = "0x06380C0e0912312B5150364B9DC4542BA0DbBc85"; // uniswap_v4 pool manager

const AMOUNT_IN = "3810485938212452485"; // step 1 amountIn

async function main() {
  // Current route: algebra → uniswap_v3 → platypus
  const currentRoute = {
    amountIn: AMOUNT_IN,
    pools: [POOL_ALG, POOL_V3_USDC_USDCE, POOL_PLAT],
    poolTypes: [1, 0, 13],
    tokens: [LINK, USDC, USDC_E, USDt],
    extraDatas: ["", "", ""],
  };
  
  // Alternative route: algebra → uniswap_v4 (USDC→USDt directly, fee=18)
  const altRoute1 = {
    amountIn: AMOUNT_IN,
    pools: [POOL_ALG, POOL_V4],
    poolTypes: [1, 9],
    tokens: [LINK, USDC, USDt],
    extraDatas: ["", "id=0xfe74ff9963652d64086e4467e64ceae7847ebf0139cb4046bcbe02c86e48f256,fee=18,ts=1,hooks=0x0000000000000000000000000000000000000000"],
  };
  
  // Alternative route: algebra → uniswap_v4 (USDC→USDt, fee=32)
  const altRoute2 = {
    amountIn: AMOUNT_IN,
    pools: [POOL_ALG, POOL_V4],
    poolTypes: [1, 9],
    tokens: [LINK, USDC, USDt],
    extraDatas: ["", "id=0x8dc096ecc5cb7565daa9615d6b6b4e6d1ffb3b16cca4e0971dfaf0ed9cb55c63,fee=32,ts=1,hooks=0x0000000000000000000000000000000000000000"],
  };

  console.log("Quoting current route (algebra→v3→platypus)...");
  try {
    const r = await quoteRoute(client as any, {
      block: BLOCK,
      inputToken: LINK,
      outputToken: USDt,
      amountIn: AMOUNT_IN,
      steps: [currentRoute],
    });
    console.log("Current route output:", r.amountOut);
  } catch(e: any) {
    console.log("Error:", e.message?.slice(0, 200));
  }
  
  console.log("\nQuoting alt route 1 (algebra→v4 fee=18)...");
  try {
    const r = await quoteRoute(client as any, {
      block: BLOCK,
      inputToken: LINK,
      outputToken: USDt,
      amountIn: AMOUNT_IN,
      steps: [altRoute1],
    });
    console.log("Alt route 1 output:", r.amountOut);
  } catch(e: any) {
    console.log("Error:", e.message?.slice(0, 200));
  }
  
  console.log("\nQuoting alt route 2 (algebra→v4 fee=32)...");
  try {
    const r = await quoteRoute(client as any, {
      block: BLOCK,
      inputToken: LINK,
      outputToken: USDt,
      amountIn: AMOUNT_IN,
      steps: [altRoute2],
    });
    console.log("Alt route 2 output:", r.amountOut);
  } catch(e: any) {
    console.log("Error:", e.message?.slice(0, 200));
  }
}

main().catch(console.error);
