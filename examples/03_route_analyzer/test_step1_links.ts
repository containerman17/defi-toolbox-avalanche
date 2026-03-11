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
const USDt = "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7";
const POOL_V4 = "0x06380C0e0912312B5150364B9DC4542BA0DbBc85";
const V4_FEE32 = "id=0x8dc096ecc5cb7565daa9615d6b6b4e6d1ffb3b16cca4e0971dfaf0ed9cb55c63,fee=32,ts=1,hooks=0x0000000000000000000000000000000000000000";
const AMOUNT_IN = 3810485938212452485n;

async function q(label: string, pools: string[], poolTypes: number[], tokens: string[], extraDatas: string[]) {
  const route = makeRoute(pools, poolTypes, tokens, extraDatas);
  try {
    const out = await quoteRoute(client, route, AMOUNT_IN, BLOCK);
    console.log(`${out} - ${label}`);
    return out;
  } catch(e: any) {
    console.log(`ERROR - ${label}: ${e.message?.slice(0, 100)}`);
    return 0n;
  }
}

async function main() {
  console.log("Target: 33844388");
  console.log("Best so far: 33843984 (V4 fee=32 after algebra)");
  console.log();
  
  // Try different LINK→USDC pools with V4(fee=32) USDt output
  // The current algebra gives 33849097 USDC from LINK
  
  // V4 fee=100 LINK→USDC
  const V4_LINK_USDC = "0x06380C0e0912312B5150364B9DC4542BA0DbBc85";
  const V4_LINK_USDC_FEE100 = "id=0x8e374367c1172c0b82aea39d32c44af6eb4e18912556e086b52008b1c8717763,fee=100,ts=1,hooks=0x0000000000000000000000000000000000000000";
  
  await q("v4(fee=100, LINK→USDC)→v4(fee=32, USDC→USDt)",
    [V4_LINK_USDC, V4_LINK_USDC], [9, 9],
    [LINK, USDC, USDt],
    [V4_LINK_USDC_FEE100, V4_FEE32]);
    
  // Actually that doesn't make sense - the pool manager address is same, but pool IDs differ
  // V4 pools are virtual pools within the pool manager
  // We can't chain two V4 calls in the router... let's check
  
  // Let me check: V4 LINK→USDC single hop first
  await q("v4(fee=100, LINK→USDC) single hop",
    [V4_LINK_USDC], [9],
    [LINK, USDC],
    [V4_LINK_USDC_FEE100]);
    
  // Currently using algebra (177a): 33849097 USDC → 33843984 USDt
  // Is there a way to get more than 33843984 USDt from LINK?
  
  // Try 3-hop: LINK→WAVAX→USDC→USDt
  const LFJ_LINK_WAVAX = "0x6f3a0c89f611ef5dc9d96650324ac633d02265d3"; // lfj_v1
  const ALG_WAVAX_USDC = "0xa02ec3ba8d17887567672b2cdcaf525534636ea0"; // algebra
  const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
  
  await q("lfj_v1(LINK→WAVAX)→algebra(WAVAX→USDC)→v4(fee=32, USDC→USDt) 3-hop",
    [LFJ_LINK_WAVAX, ALG_WAVAX_USDC, POOL_V4], [2, 1, 9],
    [LINK, WAVAX, USDC, USDt],
    ["", "", V4_FEE32]);
}

main().catch(console.error);
