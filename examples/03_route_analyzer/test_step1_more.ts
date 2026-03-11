import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";
import { quoteRoute } from "hayabusa-router";
import { type PoolType } from "hayabusa-pools";

const RPC_URL = process.env.RPC_URL || "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(RPC_URL) });

function makeRoute(pools: string[], poolTypes: number[], tokens: string[], extraDatas: string[]) {
  return pools.map((pool, i) => ({
    pool: {
      address: pool,
      providerName: "",
      poolType: poolTypes[i] as PoolType,
      tokens: [tokens[i], tokens[i + 1]],
      latestSwapBlock: 0,
      extraData: extraDatas[i] || undefined,
    },
    tokenIn: tokens[i],
    tokenOut: tokens[i + 1],
  }));
}

async function q(label: string, pools: string[], poolTypes: number[], tokens: string[], extraDatas: string[], amountIn: bigint, block: bigint) {
  const route = makeRoute(pools, poolTypes, tokens, extraDatas);
  try {
    const out = await quoteRoute(client, route, amountIn, block);
    console.log(`${out} - ${label}`);
    return out;
  } catch(e: any) {
    console.log(`ERROR - ${label}: ${e.message?.slice(0, 150)}`);
    return 0n;
  }
}

async function main() {
  const BLOCK = 80109189n;
  const LINK = "0x5947bb275c521040051d82396192181b413227a3";
  const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
  const USDC_E = "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664";
  const USDt = "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7";
  
  const ALG_177 = "0x177a7376860a04eea6bf7fb3f39bf10c49d3b42c";
  const UV3_USDC_USDCE = "0x01c7c6066ec10b1cd4821e13b9fb063680ffa083";
  const PLATYPUS = "0x5ee9008e49b922cafef9dde21446934547e42ad6";
  const POOL_V4 = "0x06380C0e0912312B5150364B9DC4542BA0DbBc85";
  const V4_FEE18 = "id=0xfe74ff9963652d64086e4467e64ceae7847ebf0139cb4046bcbe02c86e48f256,fee=18,ts=1,hooks=0x0000000000000000000000000000000000000000";
  const V4_FEE32 = "id=0x8dc096ecc5cb7565daa9615d6b6b4e6d1ffb3b16cca4e0971dfaf0ed9cb55c63,fee=32,ts=1,hooks=0x0000000000000000000000000000000000000000";
  
  const AMOUNT_IN = 3810485938212452485n;
  
  console.log("Step 1 routes (LINK → USDt):");
  console.log("Target (from original tx): ~33844388");
  console.log();
  
  await q("algebra→v3(USDC→USDC.e)→platypus(USDC.e→USDt) [CURRENT]",
    [ALG_177, UV3_USDC_USDCE, PLATYPUS], [1, 0, 13],
    [LINK, USDC, USDC_E, USDt], ["", "", ""]);
    
  await q("algebra→platypus(USDC→USDt) [skip USDC.e]",
    [ALG_177, PLATYPUS], [1, 13],
    [LINK, USDC, USDt], ["", ""]);
    
  await q("algebra→v4(fee=18, USDC→USDt)",
    [ALG_177, POOL_V4], [1, 9],
    [LINK, USDC, USDt], ["", V4_FEE18]);
    
  await q("algebra→v4(fee=32, USDC→USDt)",
    [ALG_177, POOL_V4], [1, 9],
    [LINK, USDC, USDt], ["", V4_FEE32]);
    
  // Also check: is there a different algebra pool for LINK→USDC that gives more?
  // Check all algebra pools for LINK→USDC:
  console.log("\nAlso checking: USDC amount from different LINK→USDC pools:");
  
  // The algebra pool 0x177a7376... is the one used in the original tx
  // Are there other pools?
  await q("algebra 0x177a7376 (LINK→USDC) [CURRENT]",
    [ALG_177], [1],
    [LINK, USDC], [""],
    AMOUNT_IN, BLOCK);
}

main().catch(console.error);
