import { createPublicClient, http } from 'viem';
import { avalanche } from 'viem/chains';
import { quoteRoute } from 'hayabusa-router';

const client = createPublicClient({ chain: avalanche, transport: http('http://localhost:9650/ext/bc/C/rpc') });
const USDT = '0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7';
const USDC = '0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e';
const WAVAX = '0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7';
const WOOFI = '0x4c4af8dbc524681930a27b2f1af5bcc8062e6fb7';

const uv3Pools = [
  '0x11476e10eb79ddffa6f2585be526d2bd840c3e20',
  '0x34f9235ba2328e667f0787c0c94434ebf0752d10',
  '0x1951bd56e82cc1edfec8546c3202ecbcc5b74a29',
];

for (const pool of uv3Pools) {
  try {
    const out = await quoteRoute(client, [
      { pool: { address: WOOFI, providerName: 'woofi_v2', poolType: 5, tokens: [USDT, WAVAX], latestSwapBlock: 0 }, tokenIn: USDT, tokenOut: WAVAX },
      { pool: { address: pool, providerName: 'uniswap_v3', poolType: 0, tokens: [WAVAX, USDC], latestSwapBlock: 0 }, tokenIn: WAVAX, tokenOut: USDC },
    ], 5000n, 80114945n);
    console.log(`UV3 ${pool}: ${out}`);
  } catch(e: any) {
    console.log(`UV3 ${pool} error: ${e.message?.slice(0, 80)}`);
  }
}
