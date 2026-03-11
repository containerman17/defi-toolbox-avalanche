// Test different routing options for step 1 of 447a payload
import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";
import { quoteRoute } from "hayabusa-router";
import { type StoredPool, type PoolType } from "hayabusa-pools";

const RPC_URL = process.env.RPC_URL || "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(RPC_URL) });

const BLOCK = 80109189n; // block-1
const LINK = "0x5947bb275c521040051d82396192181b413227a3";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const USDC_E = "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664";
const USDt = "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";

const AMOUNT_IN = "3810485938212452485"; // step 1 amountIn

function makeRoute(pools: string[], poolTypes: number[], tokens: string[], extraDatas: string[]): { pool: StoredPool; tokenIn: string; tokenOut: string }[] {
  return pools.map((_, i) => ({
    pool: {
      address: pools[i],
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

async function quote(label: string, pools: string[], poolTypes: number[], tokens: string[], extraDatas: string[]) {
  const route = makeRoute(pools, poolTypes, tokens, extraDatas);
  try {
    const out = await quoteRoute(client, route, BigInt(AMOUNT_IN), BLOCK);
    console.log(`${out} - ${label}`);
    return out;
  } catch(e: any) {
    console.log(`ERROR - ${label}: ${e.message?.slice(0, 100)}`);
    return 0n;
  }
}

async function main() {
  console.log("Quoting step 1 routes (LINK→USDt):");
  console.log("Expected from original tx: ~33844388");
  console.log();

  // Current payload route
  await quote("algebra→v3(USDC→USDC.e)→platypus(USDC.e→USDt) [current]",
    ["0x177a7376860a04eea6bf7fb3f39bf10c49d3b42c",
     "0x01c7c6066ec10b1cd4821e13b9fb063680ffa083",
     "0x5ee9008e49b922cafef9dde21446934547e42ad6"],
    [1, 0, 13],
    [LINK, USDC, USDC_E, USDt],
    ["", "", ""]);

  // Direct USDC→USDt via V4 fee=18
  await quote("algebra→v4(fee=18, USDC→USDt)",
    ["0x177a7376860a04eea6bf7fb3f39bf10c49d3b42c",
     "0x06380C0e0912312B5150364B9DC4542BA0DbBc85"],
    [1, 9],
    [LINK, USDC, USDt],
    ["", "id=0xfe74ff9963652d64086e4467e64ceae7847ebf0139cb4046bcbe02c86e48f256,fee=18,ts=1,hooks=0x0000000000000000000000000000000000000000"]);

  // Direct USDC→USDt via V4 fee=32
  await quote("algebra→v4(fee=32, USDC→USDt)",
    ["0x177a7376860a04eea6bf7fb3f39bf10c49d3b42c",
     "0x06380C0e0912312B5150364B9DC4542BA0DbBc85"],
    [1, 9],
    [LINK, USDC, USDt],
    ["", "id=0x8dc096ecc5cb7565daa9615d6b6b4e6d1ffb3b16cca4e0971dfaf0ed9cb55c63,fee=32,ts=1,hooks=0x0000000000000000000000000000000000000000"]);
  
  // Try algebra→v3(USDC→USDC.e)→uniswap_v4(USDC.e→USDt)
  // First check if there's a V4 pool for USDC.e→USDt
}

main().catch(console.error);
