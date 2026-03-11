// Debug each step of the 4 failing transactions
import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { quoteRoute } from "hayabusa-router";
import { type StoredPool, type PoolType } from "hayabusa-pools";
import * as fs from "node:fs";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const txHashes = [
  "0x1f84c56947f5d4e8d12096ea6c4622f368405b87f96f0924d8775453cea4bbff",
  "0x447a7f6aca1d7593abec732829422fb0ab3bce338f6b628945cb0f02f6f7cc2c",
  "0x47e00934c4c69c5f47f3b03b5f7ce3070c983205bab36e63b751504dd488a83d",
  "0xe4874588842da58d078fb918bf60b02a49eb82e14eec71113a06448a3667ae91",
];

const payloadsDir = "/home/claude/defi-toolbox-avalanche/examples/03_route_analyzer/payloads";

for (const txHash of txHashes) {
  const payload = JSON.parse(fs.readFileSync(`${payloadsDir}/${txHash}.json`, "utf-8"));
  console.log(`\n=== ${txHash.slice(0, 10)} ===`);
  console.log(`expected=${payload.expectedOut}`);
  
  if (!payload.isSplit || !payload.steps) continue;
  
  let totalOut = 0n;
  for (const step of payload.steps) {
    const route = step.pools.map((_: any, i: number) => ({
      pool: {
        address: step.pools[i],
        providerName: "",
        poolType: step.poolTypes[i] as PoolType,
        tokens: [step.tokens[i], step.tokens[i+1]],
        latestSwapBlock: 0,
        extraData: step.extraDatas[i] || undefined,
      },
      tokenIn: step.tokens[i],
      tokenOut: step.tokens[i+1],
    }));
    
    const amountIn = BigInt(step.amountIn);
    const blockNumber = BigInt(payload.block) - 1n;
    
    try {
      const out = await quoteRoute(client, route, amountIn, blockNumber);
      totalOut += out;
      const isOutputStep = step.tokens[step.tokens.length-1] === payload.outputToken ||
        (step.tokens[step.tokens.length-1] === "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664" && payload.outputToken === "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e");
      console.log(`  step ${step.route}: amountIn=${step.amountIn} -> out=${out} ${isOutputStep ? "(contributes)" : "(non-output)"}`);
    } catch (err: any) {
      console.log(`  step ${step.route}: REVERT - ${err.message?.slice(0, 120)}`);
    }
  }
  console.log(`  totalOut=${totalOut} (expected=${payload.expectedOut})`);
}
