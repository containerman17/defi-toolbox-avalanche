// Test all steps to find any that could be improved
import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";
import { quoteRoute } from "hayabusa-router";
import { type StoredPool, type PoolType } from "hayabusa-pools";
import * as fs from "node:fs";
import * as path from "node:path";

const RPC_URL = process.env.RPC_URL || "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(RPC_URL) });

function makeRoute(pools: string[], poolTypes: number[], tokens: string[], extraDatas: string[]) {
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

async function main() {
  const payloadFile = path.join(import.meta.dirname!, "payloads", "0x447a7f6aca1d7593abec732829422fb0ab3bce338f6b628945cb0f02f6f7cc2c.json");
  const payload = JSON.parse(fs.readFileSync(payloadFile, "utf-8"));
  
  const BLOCK = BigInt(payload.block) - 1n;
  const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
  const USDt = "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7";
  const POOL_V4 = "0x06380C0e0912312B5150364B9DC4542BA0DbBc85";
  const V4_FEE18 = "id=0xfe74ff9963652d64086e4467e64ceae7847ebf0139cb4046bcbe02c86e48f256,fee=18,ts=1,hooks=0x0000000000000000000000000000000000000000";
  const V4_FEE32 = "id=0x8dc096ecc5cb7565daa9615d6b6b4e6d1ffb3b16cca4e0971dfaf0ed9cb55c63,fee=32,ts=1,hooks=0x0000000000000000000000000000000000000000";

  let totalOut = 0n;
  
  for (let si = 0; si < payload.steps.length; si++) {
    const step = payload.steps[si];
    const route = makeRoute(step.pools, step.poolTypes, step.tokens, step.extraDatas);
    try {
      const out = await quoteRoute(client, route, BigInt(step.amountIn), BLOCK);
      totalOut += out;
      console.log(`Step ${si+1}: ${out} [${step.route}]`);
    } catch(e: any) {
      console.log(`Step ${si+1}: ERROR - ${e.message?.slice(0, 100)}`);
    }
  }
  
  console.log(`\nTotal: ${totalOut}`);
  console.log(`Expected: ${payload.expectedOut}`);
  console.log(`Delta: ${totalOut - BigInt(payload.expectedOut)}`);
  
  // Now try alternative for step 1 (replace algebra→v3→platypus with algebra→v4(fee=32))
  console.log("\n--- Testing alternative for step 1 ---");
  const step1 = payload.steps[0];
  const LINK = step1.tokens[0];
  
  // Alternative: algebra→v4(fee=32)
  const altRoute1 = makeRoute(
    [step1.pools[0], POOL_V4],
    [step1.poolTypes[0], 9],
    [LINK, USDC, USDt],
    ["", V4_FEE32]
  );
  
  try {
    const altOut1 = await quoteRoute(client, altRoute1, BigInt(step1.amountIn), BLOCK);
    const currentOut = await quoteRoute(client, makeRoute(step1.pools, step1.poolTypes, step1.tokens, step1.extraDatas), BigInt(step1.amountIn), BLOCK);
    const gain = altOut1 - currentOut;
    const newTotal = totalOut - currentOut + altOut1;
    console.log(`Step 1 current: ${currentOut}`);
    console.log(`Step 1 with v4(fee=32): ${altOut1} (gain: ${gain})`);
    console.log(`New total: ${newTotal}`);
    console.log(`Expected: ${payload.expectedOut}`);
    console.log(`Delta: ${newTotal - BigInt(payload.expectedOut)}`);
    console.log(`Would PASS: ${newTotal + BigInt(payload.expectedOut) / 10_000_000n >= BigInt(payload.expectedOut)}`);
  } catch(e: any) {
    console.log(`Alt step 1 error: ${e.message?.slice(0, 100)}`);
  }
}

main().catch(console.error);
