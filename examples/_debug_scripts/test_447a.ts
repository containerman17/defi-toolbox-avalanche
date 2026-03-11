import { createPublicClient, http } from 'viem';
import { avalanche } from 'viem/chains';
import { quoteRoute } from 'hayabusa-router';
import * as fs from 'node:fs';
import * as path from 'node:path';

const client = createPublicClient({ chain: avalanche, transport: http('http://localhost:9650/ext/bc/C/rpc') });

const payload = JSON.parse(fs.readFileSync(path.join(import.meta.dirname!, '03_route_analyzer/payloads/0x447a7f6aca1d7593abec732829422fb0ab3bce338f6b628945cb0f02f6f7cc2c.json'), 'utf-8'));
const blockNumber = BigInt(payload.block) - 1n;
const outputToken = payload.outputToken;

let totalOut = 0n;
for (let i = 0; i < payload.steps.length; i++) {
  const step = payload.steps[i];
  try {
    const route = step.pools.map((_, j) => ({
      pool: {
        address: step.pools[j],
        providerName: '',
        poolType: step.poolTypes[j],
        tokens: [step.tokens[j], step.tokens[j+1]],
        latestSwapBlock: 0,
        extraData: step.extraDatas[j] || undefined,
      },
      tokenIn: step.tokens[j],
      tokenOut: step.tokens[j+1],
    }));
    const amountIn = BigInt(step.amountIn);
    const out = await quoteRoute(client, route, amountIn, blockNumber);
    const finalToken = step.tokens[step.tokens.length - 1];
    const isOutput = finalToken.toLowerCase() === outputToken.toLowerCase();
    if (isOutput) totalOut += out;
    console.log(`Step ${i} OK: ${step.route.slice(0, 60)} → ${out} ${isOutput ? '← output' : ''}`);
  } catch(e: any) {
    console.log(`Step ${i} FAIL: ${step.route.slice(0, 60)} — ${e.message?.slice(0, 100)}`);
  }
}
console.log('Total:', totalOut, 'Expected:', payload.expectedOut);
