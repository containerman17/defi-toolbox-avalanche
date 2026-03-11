import { createPublicClient, http } from 'viem';
import { avalanche } from 'viem/chains';
import { quoteRoute } from 'hayabusa-router';

const client = createPublicClient({ chain: avalanche, transport: http('http://localhost:9650/ext/bc/C/rpc') });

const TOKEN_X = '0x5947bb275c521040051d82396192181b413227a3';
const USDC = '0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e';
const USDC_E = '0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664';
const USDT = '0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7';
const blockNumber = 80109189n;
const amountIn = 3810485938212452485n;

// Just the algebra step
try {
  const out = await quoteRoute(client, [
    { pool: { address: '0x177a7376860a04eea6bf7fb3f39bf10c49d3b42c', providerName: 'algebra', poolType: 1, tokens: [TOKEN_X, USDC], latestSwapBlock: 0 }, tokenIn: TOKEN_X, tokenOut: USDC },
  ], amountIn, blockNumber);
  console.log('algebra step result:', out, 'USDC');
  // Now try UV3 USDC->USDC.e with that amount
  const usdcAmt = out;
  const usdceAmt = await quoteRoute(client, [
    { pool: { address: '0x01c7c6066ec10b1cd4821e13b9fb063680ffa083', providerName: 'uniswap_v3', poolType: 0, tokens: [USDC, USDC_E], latestSwapBlock: 0 }, tokenIn: USDC, tokenOut: USDC_E },
  ], usdcAmt, blockNumber);
  console.log('uv3 usdc->usdce result:', usdceAmt, 'USDC.e');
  // Now pharaoh 
  const pharaohOut = await quoteRoute(client, [
    { pool: { address: '0x6f51a46052529fe8104717e392965b2e17cef4f2', providerName: 'pharaoh_v1', poolType: 7, tokens: [USDC_E, USDT], latestSwapBlock: 0 }, tokenIn: USDC_E, tokenOut: USDT },
  ], usdceAmt, blockNumber);
  console.log('pharaoh result:', pharaohOut, 'USDt');
} catch(e: any) {
  console.log('Error:', e.message?.slice(0, 200));
}
