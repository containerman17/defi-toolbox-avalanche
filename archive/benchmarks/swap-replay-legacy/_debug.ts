import { createPublicClient, http, keccak256, pad, toHex, encodeFunctionData, type Hex, decodeAbiParameters } from 'viem';
import { avalanche } from 'viem/chains';
import { getRouterBytecode, buildStateOverrides, getBalanceOverride } from './lib/overrides.ts';
import { encodeSwap } from './lib/encode.ts';
import { type PoolType } from '../../tools/pool-collector/index.ts';

const BACKRUN_ROUTER = '0x000000000000000000000000cafebabe00facade';
const DUMMY_SENDER = '0x000000000000000000000000000000000000dEaD';

const client = createPublicClient({
  chain: avalanche,
  transport: http('http://localhost:9650/ext/bc/C/rpc'),
});

async function main() {
  const token = '0x7b640a60daa4ee5fbc2ce81797c11d174daa3b4f';
  const pool = '0x1be6b5d2c5cf0f396d78e58c95bfb795f3d7b253';
  const wavax = '0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7';
  const usdc = '0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e';
  const amountIn = 250000000000000000000n;
  const block = 80112976n;

  // Try setting balance for BOTH sender and router
  const route = [
    {
      pool: { address: pool, providerName: '', poolType: 2 as PoolType, tokens: [token, wavax], latestSwapBlock: 0 },
      tokenIn: token, tokenOut: wavax,
    },
    {
      pool: { address: '0xfae3f424a0a47706811521e3ee268f00cfb5c45e', providerName: '', poolType: 0 as PoolType, tokens: [wavax, usdc], latestSwapBlock: 0 },
      tokenIn: wavax, tokenOut: usdc,
    },
  ];
  const calldata = encodeSwap(route, amountIn);
  const tokenAmounts = new Map<string, bigint>([[token.toLowerCase(), amountIn]]);
  const code = getRouterBytecode();

  // Build state override with EXTRA: also set balance for router
  const stateOverride = buildStateOverrides({
    routerAddress: BACKRUN_ROUTER,
    tokenAmounts,
    extraStateOverrides: { [BACKRUN_ROUTER]: { code } },
  });

  // Add router balance override for the token
  const routerBalOvr = getBalanceOverride(token, amountIn, BACKRUN_ROUTER);
  for (const [addr, val] of Object.entries(routerBalOvr)) {
    if (stateOverride[addr]) {
      if (stateOverride[addr].stateDiff) {
        Object.assign(stateOverride[addr].stateDiff, val.stateDiff);
      } else {
        stateOverride[addr].stateDiff = val.stateDiff;
      }
    } else {
      stateOverride[addr] = val;
    }
  }

  try {
    const result = await client.request({
      method: 'eth_call' as any,
      params: [
        { from: DUMMY_SENDER, to: BACKRUN_ROUTER, data: calldata },
        `0x${block.toString(16)}`,
        stateOverride,
      ] as any,
    });
    const [amountOut] = decodeAbiParameters([{type:'uint256'}], result as Hex);
    console.log(`Full route with router balance override: amountOut = ${amountOut} (expected ~205909714)`);
  } catch (e: any) {
    console.log(`FAILED: ${e.details || e.message?.slice(0, 300)}`);
  }

  // Also try single hop only
  const route1 = [route[0]];
  const calldata1 = encodeSwap(route1, amountIn);
  try {
    const result = await client.request({
      method: 'eth_call' as any,
      params: [
        { from: DUMMY_SENDER, to: BACKRUN_ROUTER, data: calldata1 },
        `0x${block.toString(16)}`,
        stateOverride,
      ] as any,
    });
    const [amountOut] = decodeAbiParameters([{type:'uint256'}], result as Hex);
    console.log(`Single hop with router balance override: amountOut = ${amountOut}`);
  } catch (e: any) {
    console.log(`Single hop FAILED: ${e.details || e.message?.slice(0, 300)}`);
  }
}

main().catch(console.error);
