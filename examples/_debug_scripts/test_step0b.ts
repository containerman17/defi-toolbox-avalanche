import { createPublicClient, http } from 'viem';
import { avalanche } from 'viem/chains';
import { quoteRoute } from 'hayabusa-router';

const client = createPublicClient({ chain: avalanche, transport: http('http://localhost:9650/ext/bc/C/rpc') });

const TOKEN_X = '0x5947bb275c521040051d82396192181b413227a3';
const USDC = '0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e';
const USDC_E = '0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664';
const USDT = '0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7';
const blockNumber = 80109189n;

// Test different UV3 pools for USDC->USDC.e
const uv3Pools = [
  '0x01c7c6066ec10b1cd4821e13b9fb063680ffa083', // block 80503530 (current pick)
  '0x6a069a8cab98803e6785453041e0ceba57e0138c', // block 80499012
];

for (const uv3Pool of uv3Pools) {
  const step0 = [
    { pool: { address: '0x177a7376860a04eea6bf7fb3f39bf10c49d3b42c', providerName: 'algebra', poolType: 1, tokens: [TOKEN_X, USDC], latestSwapBlock: 0 }, tokenIn: TOKEN_X, tokenOut: USDC },
    { pool: { address: uv3Pool, providerName: 'uniswap_v3', poolType: 0, tokens: [USDC, USDC_E], latestSwapBlock: 0 }, tokenIn: USDC, tokenOut: USDC_E },
    { pool: { address: '0x6f51a46052529fe8104717e392965b2e17cef4f2', providerName: 'pharaoh_v1', poolType: 7, tokens: [USDC_E, USDT], latestSwapBlock: 0 }, tokenIn: USDC_E, tokenOut: USDT },
  ];
  try {
    const out = await quoteRoute(client, step0, 3810485938212452485n, blockNumber);
    console.log(`UV3 ${uv3Pool.slice(0,10)}: step0 result=${out}`);
  } catch(e: any) {
    console.log(`UV3 ${uv3Pool.slice(0,10)}: error=${e.message?.slice(0,80)}`);
  }
}
