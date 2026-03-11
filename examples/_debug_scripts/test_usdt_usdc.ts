import { createPublicClient, http, encodeFunctionData } from 'viem';
import { avalanche } from 'viem/chains';
import { quoteRoute, ROUTER_ADDRESS } from 'hayabusa-router';

const client = createPublicClient({ chain: avalanche, transport: http('http://localhost:9650/ext/bc/C/rpc') });
const USDT = '0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7';
const USDC = '0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e';
const WAVAX = '0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7';
const WOOFI = '0x4c4af8dbc524681930a27b2f1af5bcc8062e6fb7';
const UV3_WAVAX_USDC = '0x34f9235ba2328e667f0787c0c94434ebf0752d10';

// Try WooFi USDt->WAVAX, then UV3 WAVAX->USDC
try {
  const out = await quoteRoute(client, [
    { pool: { address: WOOFI, providerName: 'woofi_v2', poolType: 5, tokens: [USDT, WAVAX], latestSwapBlock: 0 }, tokenIn: USDT, tokenOut: WAVAX },
    { pool: { address: UV3_WAVAX_USDC, providerName: 'uniswap_v3', poolType: 0, tokens: [WAVAX, USDC], latestSwapBlock: 0 }, tokenIn: WAVAX, tokenOut: USDC },
  ], 5000n, 80114945n);
  console.log('WooFi+UV3 USDt->WAVAX->USDC:', out.toString());
} catch(e: any) {
  console.log('WooFi+UV3 error:', e.message?.slice(0, 150));
}

// Try direct WooFi USDt->USDC (WooFi supports multiple tokens)
try {
  const out = await quoteRoute(client, [
    { pool: { address: WOOFI, providerName: 'woofi_v2', poolType: 5, tokens: [USDT, USDC], latestSwapBlock: 0 }, tokenIn: USDT, tokenOut: USDC },
  ], 5000n, 80114945n);
  console.log('WooFi direct USDt->USDC:', out.toString());
} catch(e: any) {
  console.log('WooFi direct error:', e.message?.slice(0, 150));
}
