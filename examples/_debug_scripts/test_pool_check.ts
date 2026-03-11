import { createPublicClient, http, decodeAbiParameters } from 'viem';
import { avalanche } from 'viem/chains';

const client = createPublicClient({ chain: avalanche, transport: http('http://localhost:9650/ext/bc/C/rpc') });

// Check pool 0x0711b6026068f736bae6b213031fce978d48e026 tokens at block 80114946
const POOL = '0x0711b6026068f736bae6b213031fce978d48e026';
const VAULT = '0xba1333333333a1ba1108e8412f11850a5c319ba9';

// Try getPoolTokens at BLOCK 80114946
try {
  const result = await client.call({
    to: VAULT,
    data: ('0xca4f2803' + '000000000000000000000000' + POOL.slice(2)) as `0x${string}`,
    blockNumber: 80114946n
  });
  console.log('Pool tokens at 80114946:', result.data);
  if (result.data && result.data !== '0x') {
    const [tokens] = decodeAbiParameters([{ type: 'address[]' }], result.data);
    console.log('tokens:', tokens);
  }
} catch(e: any) {
  console.log('getPoolTokens error at 80114946:', e.message?.slice(0, 100));
}

// Let's look at what contract it is
const code = await client.getCode({ address: POOL as `0x${string}`, blockNumber: 80114946n });
console.log('Code length:', (code?.length ?? 2)/2 - 1, 'bytes');
