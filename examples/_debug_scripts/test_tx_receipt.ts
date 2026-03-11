import { createPublicClient, http } from 'viem';
import { avalanche } from 'viem/chains';

const client = createPublicClient({ chain: avalanche, transport: http('http://localhost:9650/ext/bc/C/rpc') });

const receipt = await client.getTransactionReceipt({ hash: '0xa4611a51fe69a88ed4ecf602ce7c8b8cfaea07d81f80d1a86aad4ffa32bf3aa8' });
console.log('block:', receipt.blockNumber);
console.log('logs:', receipt.logs.map(l => ({ addr: l.address, topics: l.topics, data: l.data?.slice(0,100) })));
