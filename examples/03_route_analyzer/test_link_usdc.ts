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
const USDt = "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7";
const POOL_V4 = "0x06380C0e0912312B5150364B9DC4542BA0DbBc85";
const V4_FEE32 = "id=0x8dc096ecc5cb7565daa9615d6b6b4e6d1ffb3b16cca4e0971dfaf0ed9cb55c63,fee=32,ts=1,hooks=0x0000000000000000000000000000000000000000";
const AMOUNT_IN = 3810485938212452485n;

async function main() {
  const poolsPath = path.join(import.meta.dirname!, "../../pools/data/pools.txt");
  const { pools: poolMap } = loadPools(poolsPath);
  
  console.log("LINK→USDC pool survey for 3810485938212452485 LINK:");
  
  const results: { out: bigint; label: string; pool: string; poolType: number; extraData: string }[] = [];
  
  for (const [addr, p] of poolMap) {
    const toks = p.tokens.map(t => t.toLowerCase());
    if (!toks.includes(LINK) || !toks.includes(USDC)) continue;
    
    if (p.poolType === 6 || p.poolType === 10 || p.poolType === 11) continue;
    
    let extraData = p.extraData || "";
    let route;
    
    if (p.poolType === 9) {
      // V4 pool - use address as the virtual pool id
      extraData = `id=${addr}${p.extraData?.startsWith("id=") ? "," + p.extraData.split(",").slice(1).join(",") : (p.extraData || "")}`;
      // Actually extraData already has the id in the pools.txt format "@id=..."
      const rawExtra = (p as any).rawExtraData || "";
      if (!rawExtra) continue;
      route = makeRoute([POOL_V4], [9], [LINK, USDC], [rawExtra]);
    } else {
      route = makeRoute([p.address], [p.poolType], [LINK, USDC], [extraData]);
    }
    
    try {
      const out = await quoteRoute(client, route, AMOUNT_IN, BLOCK);
      results.push({ out, label: `${p.providerName}(${p.address.slice(0,10)}) type=${p.poolType}`, pool: p.address, poolType: p.poolType, extraData });
    } catch {}
  }
  
  // Now check V4 pools manually
  const V4_LINK_USDC_POOLS = [
    { label: "V4 fee=100 ts=1", extra: "id=0x8e374367c1172c0b82aea39d32c44af6eb4e18912556e086b52008b1c8717763,fee=100,ts=1,hooks=0x0000000000000000000000000000000000000000" },
    { label: "V4 fee=900000", extra: "id=0xf1dddcfdf3016f1ff166188acbe664ded5d20bde2e9ad84af5fd985ef3569490,fee=900000,ts=18000,hooks=0x0000000000000000000000000000000000000000" },
  ];
  
  for (const { label, extra } of V4_LINK_USDC_POOLS) {
    try {
      const out = await quoteRoute(client, makeRoute([POOL_V4], [9], [LINK, USDC], [extra]), AMOUNT_IN, BLOCK);
      results.push({ out, label: `V4 ${label} LINK→USDC`, pool: POOL_V4, poolType: 9, extraData: extra });
    } catch {}
  }
  
  results.sort((a, b) => (b.out > a.out ? 1 : -1));
  console.log("Top 10 LINK→USDC:");
  for (const r of results.slice(0, 10)) {
    console.log(`${r.out} USDC - ${r.label}`);
  }
  
  // Now test the best LINK→USDC pool followed by V4 fee=32 USDC→USDt
  console.log("\nTop 5 LINK→USDC→USDt combined:");
  const combined: { out: bigint; label: string }[] = [];
  for (const r of results.slice(0, 5)) {
    const route2 = makeRoute([r.pool, POOL_V4], [r.poolType, 9], [LINK, USDC, USDt], [r.extraData, V4_FEE32]);
    try {
      const out = await quoteRoute(client, route2, AMOUNT_IN, BLOCK);
      combined.push({ out, label: `${r.label} → V4(fee=32)` });
    } catch(e: any) {
      combined.push({ out: 0n, label: `ERROR: ${r.label} → V4(fee=32): ${e.message?.slice(0,50)}` });
    }
  }
  combined.sort((a, b) => (b.out > a.out ? 1 : -1));
  for (const c of combined) {
    console.log(`${c.out} USDt - ${c.label}`);
  }
}

main().catch(console.error);
