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

async function q(pools: string[], poolTypes: number[], tokens: string[], extraDatas: string[], amountIn: bigint, block: bigint) {
  const route = makeRoute(pools, poolTypes, tokens, extraDatas);
  return await quoteRoute(client, route, amountIn, block);
}

async function main() {
  const BLOCK = 80109189n; // block-1
  const USDt = "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7";
  const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
  const POOL_V4 = "0x06380C0e0912312B5150364B9DC4542BA0DbBc85";
  const V4_FEE18 = "id=0xfe74ff9963652d64086e4467e64ceae7847ebf0139cb4046bcbe02c86e48f256,fee=18,ts=1,hooks=0x0000000000000000000000000000000000000000";
  const V4_FEE32 = "id=0x8dc096ecc5cb7565daa9615d6b6b4e6d1ffb3b16cca4e0971dfaf0ed9cb55c63,fee=32,ts=1,hooks=0x0000000000000000000000000000000000000000";

  // Single-hop USDC→USDt via V4 pool
  async function cmpFees(label: string, amtIn: bigint) {
    const r18 = await q([POOL_V4], [9], [USDC, USDt], [V4_FEE18], amtIn, BLOCK);
    const r32 = await q([POOL_V4], [9], [USDC, USDt], [V4_FEE32], amtIn, BLOCK);
    console.log(`${label}: fee18=${r18} fee32=${r32} diff=${r32 - r18}`);
    return { r18, r32 };
  }

  const s3 = await cmpFees("Step 3 (14874712 USDC)", 14874712n);
  const s5 = await cmpFees("Step 5 (44555042 USDC)", 44555042n);
  const s8 = await cmpFees("Step 8 (6756303 USDC)", 6756303n);
  const s11 = await cmpFees("Step 11 (20247098 USDC)", 20247098n);
  const s14 = await cmpFees("Step 14 (12147386 USDC)", 12147386n);
  const s19 = await cmpFees("Step 19 (1349894 USDC)", 1349894n);
  
  console.log("\nStep 3 currently uses fee18.");
  console.log("Steps 5,8,11,14,19 currently use fee32.");
  
  // The current total from USDt steps:
  // Step1: 33822787, Step3: 14872465(fee18), Step5: 44548312(fee32), Step8: 6755281(fee32)
  // Step11: 20244040(fee32), Step14: 12145551(fee32), Step16: 1350935, Step19: 1349689(fee32)
  // Total = 135089060
  
  // If we change Step1 to algebra→v4(fee32) and Step3 to fee32:
  const LINK = "0x5947bb275c521040051d82396192181b413227a3";
  const USDC_E = "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664";
  const step1_alt = await q(
    ["0x177a7376860a04eea6bf7fb3f39bf10c49d3b42c", POOL_V4],
    [1, 9],
    [LINK, USDC, USDt],
    ["", V4_FEE32],
    3810485938212452485n, BLOCK
  );
  
  console.log(`\nStep 1 current (algebra→v3→platypus): 33822787`);
  console.log(`Step 1 alt (algebra→v4 fee32): ${step1_alt}`);
  
  // If step3 switches to fee32:
  const step3_fee32 = s3.r32;
  const step3_fee18_current = s3.r18;  // should be 14872465
  
  const newTotal = 135089060n - 33822787n + step1_alt - step3_fee18_current + step3_fee32;
  console.log(`\nNew total (step1→v4fee32, step3→fee32): ${newTotal}`);
  console.log(`Expected: 135110659`);
  console.log(`Delta: ${newTotal - 135110659n}`);
  console.log(`Would PASS: ${newTotal + 135110659n / 10000000n >= 135110659n}`);
}

main().catch(console.error);
