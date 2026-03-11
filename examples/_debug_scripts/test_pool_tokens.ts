import { createPublicClient, http, decodeAbiParameters } from 'viem';
import { avalanche } from 'viem/chains';

const client = createPublicClient({ chain: avalanche, transport: http('http://localhost:9650/ext/bc/C/rpc') });

const BALANCER_V3_VAULT = '0xba1333333333a1ba1108e8412f11850a5c319ba9';
const POOL = '0x0711b6026068f736bae6b213031fce978d48e026';

try {
  const result = await client.call({
    to: BALANCER_V3_VAULT,
    // getPoolTokens(address) selector 0xca4f2803
    data: ('0xca4f2803' + '000000000000000000000000' + POOL.slice(2)) as `0x${string}`,
  });
  console.log('raw result:', result.data);
  if (result.data) {
    const [tokens] = decodeAbiParameters([{ type: 'address[]' }], result.data);
    console.log('tokens:', tokens);
  }
} catch(e: any) {
  console.log('getPoolTokens error:', e.message?.slice(0, 200));
}

// Also check the second address from the task
const POOL2 = '0x5f1e8ed833fd69723f28bde26ebe65e74d791c4e';
try {
  const code = await client.getCode({ address: POOL2 });
  console.log('pool2 code length:', (code?.length ?? 2) / 2 - 1, 'bytes');
} catch(e: any) {
  console.log('pool2 error:', e.message);
}

// And the other pool from tx receipt
