import { createPublicClient, http, encodeFunctionData, encodeAbiParameters } from 'viem';
import { avalanche } from 'viem/chains';

const client = createPublicClient({ chain: avalanche, transport: http('http://localhost:9650/ext/bc/C/rpc') });

// Let's check what on-chain BalV3 pool 0xa4e1b0 looks like at block 80114945
const BALANCER_V3_VAULT = '0xba1333333333a1ba1108e8412f11850a5c319ba9';

// getPoolTokens selector: 0xddf252... no, let me use the right one
// Actually let me try a simpler buffered swap to see if the mechanism works at all
// Try just a direct USDt->USDC swap that avoids the woofi step
// First, check USDt pool
try {
  // Query pool tokens using getPoolTokens(address)
  const result = await client.call({
    to: BALANCER_V3_VAULT,
    data: ('0xca4f2803' + '000000000000000000000000a4e1b0ddffc0e3aa63dbca462cf370e4f1dc9b8b') as `0x${string}`,
    blockNumber: 80114945n
  });
  console.log('pool tokens result:', result.data);
} catch(e: any) {
  console.log('pool tokens error:', e.message?.slice(0, 200));
}
