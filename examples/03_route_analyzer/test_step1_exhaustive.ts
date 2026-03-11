import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";
import { quoteRoute } from "hayabusa-router";
import { type PoolType, loadPools } from "hayabusa-pools";
import * as path from "node:path";

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
const AMOUNT_IN = 3810485938212452485n;

async function main() {
  const poolsPath = path.join(import.meta.dirname!, "../../pools/data/pools.txt");
  const pools = loadPools(poolsPath);
  
  const POOL_V4 = "0x06380C0e0912312B5150364B9DC4542BA0DbBc85";
  const V4_FEE18 = "id=0xfe74ff9963652d64086e4467e64ceae7847ebf0139cb4046bcbe02c86e48f256,fee=18,ts=1,hooks=0x0000000000000000000000000000000000000000";
  const V4_FEE32 = "id=0x8dc096ecc5cb7565daa9615d6b6b4e6d1ffb3b16cca4e0971dfaf0ed9cb55c63,fee=32,ts=1,hooks=0x0000000000000000000000000000000000000000";
  
  console.log("Testing all USDC→USDt and USDC.e→USDt pools (after algebra LINK→USDC)...");
  console.log("Target: 33844388");
  console.log();
  
  const results: { out: bigint; label: string }[] = [];
  
  // Test V4 directly
  const v4_fee18 = makeRoute([ALG_177, POOL_V4], [1, 9], [LINK, USDC, USDt], ["", V4_FEE18]);
  const v4_fee32 = makeRoute([ALG_177, POOL_V4], [1, 9], [LINK, USDC, USDt], ["", V4_FEE32]);
  
  try {
    const o1 = await quoteRoute(client, v4_fee18, AMOUNT_IN, BLOCK);
    results.push({ out: o1, label: "V4 fee=18 (USDC→USDt)" });
  } catch {}
  
  try {
    const o2 = await quoteRoute(client, v4_fee32, AMOUNT_IN, BLOCK);
    results.push({ out: o2, label: "V4 fee=32 (USDC→USDt)" });
  } catch {}
  
  // Test all pools that have USDC.e and USDt
  for (const p of pools) {
    const toks = p.tokens.map(t => t.toLowerCase());
    const hasUsdce = toks.includes(USDC_E);
    const hasUsdt = toks.includes(USDt);
    const hasUsdc = toks.includes(USDC);
    
    if ((!hasUsdce && !hasUsdc) || !hasUsdt) continue;
    if (p.poolType === 9 || p.poolType === 6 || p.poolType === 10 || p.poolType === 11) continue; // Skip complex types
    
    // Try USDC→USDt if both present
    if (hasUsdc && hasUsdt) {
      const route = makeRoute([ALG_177, p.address], [1, p.poolType], [LINK, USDC, USDt], ["", p.extraData || ""]);
      try {
        const out = await quoteRoute(client, route, AMOUNT_IN, BLOCK);
        results.push({ out, label: `${p.providerName}(${p.address.slice(0,10)}) USDC→USDt` });
      } catch(e: any) {
        // console.log(`Skip ${p.providerName}: ${e.message?.slice(0,50)}`);
      }
    }
    
    // Try USDC.e→USDt if both present (with bridge hop)
    if (hasUsdce && hasUsdt) {
      // Need to find a USDC→USDC.e bridge
      const UV3_USDC_USDCE = "0x01c7c6066ec10b1cd4821e13b9fb063680ffa083";
      const route = makeRoute([ALG_177, UV3_USDC_USDCE, p.address], [1, 0, p.poolType], [LINK, USDC, USDC_E, USDt], ["", "", p.extraData || ""]);
      try {
        const out = await quoteRoute(client, route, AMOUNT_IN, BLOCK);
        results.push({ out, label: `v3→${p.providerName}(${p.address.slice(0,10)}) USDC.e→USDt` });
      } catch(e: any) {
        // Skip
      }
    }
  }
  
  // Sort by output descending
  results.sort((a, b) => (b.out > a.out ? 1 : -1));
  for (const r of results.slice(0, 15)) {
    console.log(`${r.out} - ${r.label}`);
  }
}

main().catch(console.error);
