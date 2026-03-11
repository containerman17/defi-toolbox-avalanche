import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";
import { quoteRoute } from "hayabusa-router";
import { type StoredPool, type PoolType } from "hayabusa-pools";
import * as fs from "node:fs";

const rpcUrl = "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

async function testPayload(filePath: string) {
  const payload = JSON.parse(fs.readFileSync(filePath, "utf-8"));
  const blockNumber = BigInt(payload.block) - 1n;
  console.log(`\n=== ${payload.txHash.slice(0, 10)} block=${payload.block} ===`);
  console.log(`expected: ${payload.expectedOut}`);

  let totalOut = 0n;
  for (const step of payload.steps) {
    const route = step.pools.map((_: string, i: number) => ({
      pool: {
        address: step.pools[i],
        providerName: "",
        poolType: step.poolTypes[i] as PoolType,
        tokens: [step.tokens[i], step.tokens[i + 1]],
        latestSwapBlock: 0,
        extraData: step.extraDatas[i] || undefined,
      },
      tokenIn: step.tokens[i],
      tokenOut: step.tokens[i + 1],
    }));
    
    try {
      const out = await quoteRoute(client, route, BigInt(step.amountIn), blockNumber);
      const outputToken = payload.outputToken;
      const lastToken = step.tokens[step.tokens.length - 1];
      const countable = lastToken === outputToken || 
        (lastToken === "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664" && outputToken === "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e");
      if (countable) totalOut += out;
      console.log(`  ${step.route}: in=${step.amountIn} out=${out}`);
    } catch (err: any) {
      console.log(`  ${step.route}: REVERT - ${err.message?.slice(0, 100)}`);
    }
  }
  console.log(`  total: ${totalOut} (expected: ${payload.expectedOut}) delta=${totalOut - BigInt(payload.expectedOut)}`);
}

const base = "/home/claude/defi-toolbox-avalanche/examples/03_route_analyzer/payloads/";
const payloads = [
  "0x1f84c56947f5d4e8d12096ea6c4622f368405b87f96f0924d8775453cea4bbff.json",
  "0x447a7f6aca1d7593abec732829422fb0ab3bce338f6b628945cb0f02f6f7cc2c.json",
  "0x47e00934c4c69c5f47f3b03b5f7ce3070c983205bab36e63b751504dd488a83d.json",
  "0xe4874588842da58d078fb918bf60b02a49eb82e14eec71113a06448a3667ae91.json",
];

for (const p of payloads) {
  await testPayload(base + p);
}
