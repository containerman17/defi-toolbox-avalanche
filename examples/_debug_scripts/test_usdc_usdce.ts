import { createPublicClient, http } from 'viem';
import { avalanche } from 'viem/chains';
import { quoteRoute } from 'hayabusa-router';

const client = createPublicClient({ chain: avalanche, transport: http('http://localhost:9650/ext/bc/C/rpc') });
const USDC = '0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e';
const USDC_E = '0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664';

// Test pool 0x01c7c606 for USDC->USDC.e (block 80503530)
try {
  const out = await quoteRoute(client, [
    { pool: { address: '0x01c7c6066ec10b1cd4821e13b9fb063680ffa083', providerName: 'uniswap_v3', poolType: 0, tokens: [USDC, USDC_E], latestSwapBlock: 0 }, tokenIn: USDC, tokenOut: USDC_E },
  ], 1000000n, 80503540n);
  console.log('UV3 0x01c7c6: USDC->USDC.e result:', out);
} catch(e: any) {
  console.log('error:', e.message?.slice(0, 100));
}
