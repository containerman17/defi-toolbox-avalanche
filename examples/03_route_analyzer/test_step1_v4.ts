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
const V4_FEE10000 = "id=0x04ee6925b6e8fc7525cc73e578e7b7ef6ea1470ff0d9a5f71e178707599344a3,fee=10000,ts=200,hooks=0x0000000000000000000000000000000000000000";

const AMOUNT_IN = 3810485938212452485n;

async function q(label: string, pools: string[], poolTypes: number[], tokens: string[], extraDatas: string[]) {
  const route = makeRoute(pools, poolTypes, tokens, extraDatas);
  try {
    const out = await quoteRoute(client, route, AMOUNT_IN, BLOCK);
    console.log(`${out} - ${label}`);
    return out;
  } catch(e: any) {
    console.log(`ERROR - ${label}: ${e.message?.slice(0, 150)}`);
    return 0n;
  }
}

async function main() {
  console.log("Target: 33844388\n");
  
  // Try V4 USDC.e→USDt (fee=10000) with bridge
  await q("algebra→v3(USDC→USDC.e)→v4(USDC.e→USDt fee=10000)",
    [ALG_177, UV3_USDC_USDCE, POOL_V4], [1, 0, 9],
    [LINK, USDC, USDC_E, USDt],
    ["", "", V4_FEE10000]);
    
  // Try USDC.e→USDt directly via V4 (fee18)
  await q("algebra→v3(USDC→USDC.e)→v4(USDC.e→USDt fee=18)",
    [ALG_177, UV3_USDC_USDCE, POOL_V4], [1, 0, 9],
    [LINK, USDC, USDC_E, USDt],
    ["", "", V4_FEE18]);
    
  // Try USDC.e→USDt directly via V4 (fee32)
  await q("algebra→v3(USDC→USDC.e)→v4(USDC.e→USDt fee=32)",
    [ALG_177, UV3_USDC_USDCE, POOL_V4], [1, 0, 9],
    [LINK, USDC, USDC_E, USDt],
    ["", "", V4_FEE32]);
    
  // Try platypus USDC→USDt directly (skip the bridge)
  await q("algebra→platypus(USDC→USDt direct)",
    [ALG_177, PLATYPUS], [1, 13],
    [LINK, USDC, USDt], ["", ""]);
    
  // What if we skip the USDC.e bridge? Already tried algebra→v4(fee=32)
  // Best so far is 33843984
  
  // Check if there's another way to get +404 more
  // Try using the UniV3 USDC.e→USDt pool (0x6f51a...)
  const PHARAOH_V1 = "0x6f51a46052529fe8104717e392965b2e17cef4f2";
  await q("algebra→v3(USDC→USDC.e)→pharaoh_v1(USDC.e→USDt)",
    [ALG_177, UV3_USDC_USDCE, PHARAOH_V1], [1, 0, 7],
    [LINK, USDC, USDC_E, USDt],
    ["", "", ""]);
    
  await q("algebra→pharaoh_v1(USDC→USDt direct)",
    [ALG_177, PHARAOH_V1], [1, 7],
    [LINK, USDC, USDt], ["", ""]);
}

main().catch(console.error);
