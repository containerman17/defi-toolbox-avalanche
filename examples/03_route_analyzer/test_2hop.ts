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
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const USDt = "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7";
const POOL_V4 = "0x06380C0e0912312B5150364B9DC4542BA0DbBc85";
const V4_FEE18 = "id=0xfe74ff9963652d64086e4467e64ceae7847ebf0139cb4046bcbe02c86e48f256,fee=18,ts=1,hooks=0x0000000000000000000000000000000000000000";
const V4_FEE32 = "id=0x8dc096ecc5cb7565daa9615d6b6b4e6d1ffb3b16cca4e0971dfaf0ed9cb55c63,fee=32,ts=1,hooks=0x0000000000000000000000000000000000000000";

// We have 33849097 USDC to convert to USDt
// Target: >= 33844388 USDt
const USDC_AMOUNT = 33849097n;

async function main() {
  const poolsPath = path.join(import.meta.dirname!, "../../pools/data/pools.txt");
  const { pools: poolMap } = loadPools(poolsPath);
  
  console.log(`Converting ${USDC_AMOUNT} USDC → USDt`);
  console.log(`Target: >= 33844388`);
  console.log();
  
  // Direct V4 (best known)
  const v4_18 = await quoteRoute(client, makeRoute([POOL_V4], [9], [USDC, USDt], [V4_FEE18]), USDC_AMOUNT, BLOCK);
  const v4_32 = await quoteRoute(client, makeRoute([POOL_V4], [9], [USDC, USDt], [V4_FEE32]), USDC_AMOUNT, BLOCK);
  console.log(`V4 fee=18: ${v4_18}`);
  console.log(`V4 fee=32: ${v4_32}`);
  
  // Try 2-hop: USDC→USDT.e→USDt or USDC→DAI→USDt etc.
  const USDC_E = "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664";
  const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
  const USDT_E = "0xc7198437980c041c805a1edcba50c1ce5db95118";
  
  // 2-hop via USDC.e intermediate: USDC→USDC.e→USDt via platypus
  // But platypus 0x5ee9008e gives 33822787 for 3810485938... LINK (which is ~33849097 USDC)
  // So USDC→USDC.e→USDt would give approximately same as USDC→platypus directly
  
  // Let's try: using Platypus directly for USDC→USDt (without bridge)
  const PLATYPUS = "0x5ee9008e49b922cafef9dde21446934547e42ad6";
  try {
    const r = await quoteRoute(client, makeRoute([PLATYPUS], [13], [USDC, USDt], [""]), USDC_AMOUNT, BLOCK);
    console.log(`Platypus (USDC→USDt direct): ${r}`);
  } catch(e: any) {
    console.log(`Platypus (USDC→USDt direct): ERROR - ${e.message?.slice(0,80)}`);
  }
  
  // Try: USDC→USDC.e (V3)→platypus (USDC.e→USDt)
  const V3_USDC_USDCE = "0x01c7c6066ec10b1cd4821e13b9fb063680ffa083";
  try {
    const r = await quoteRoute(client, makeRoute([V3_USDC_USDCE, PLATYPUS], [0, 13], [USDC, USDC_E, USDt], ["", ""]), USDC_AMOUNT, BLOCK);
    console.log(`V3(USDC→USDC.e)→Platypus(USDC.e→USDt): ${r}`);
  } catch(e: any) {
    console.log(`V3→Platypus: ERROR - ${e.message?.slice(0,80)}`);
  }
  
  // Try: USDC→USDt via WooFi
  const WOOFI = "0x4c4af8dbc524681930a27b2f1af5bcc8062e6fb7";
  for (const [addr, p] of poolMap) {
    if (p.providerName !== "woofi_v2") continue;
    const toks = p.tokens.map(t => t.toLowerCase());
    if (!toks.includes(USDC) || !toks.includes(USDt)) continue;
    try {
      const r = await quoteRoute(client, makeRoute([p.address], [p.poolType], [USDC, USDt], [p.extraData || ""]), USDC_AMOUNT, BLOCK);
      console.log(`WooFi(${p.address.slice(0,10)}) USDC→USDt: ${r}`);
    } catch {}
  }
}

main().catch(console.error);
