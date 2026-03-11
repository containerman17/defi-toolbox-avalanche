import { createPublicClient, http, encodeFunctionData, encodeAbiParameters } from 'viem';
import { avalanche } from 'viem/chains';

const client = createPublicClient({ chain: avalanche, transport: http('http://localhost:9650/ext/bc/C/rpc') });

const extraData = encodeAbiParameters(
  [{type:'address'},{type:'address'},{type:'address'}],
  ['0xd7da0de6ef4f51d6206bf2a35fcd2030f54c3f7b','0xa4e1b0ddffc0e3aa63dbca462cf370e4f1dc9b8b','0xe1bfc96d95badcb10ff013cb0c9c6c737ca07009']
);
console.log('extraData:', extraData);

try {
  const result = await client.call({
    to: '0x06380C0e0912312B5150364B9DC4542BA0DbBc85',
    data: encodeFunctionData({
      abi: [{name:'quoteRoute',type:'function',inputs:[{name:'pools',type:'address[]'},{name:'poolTypes',type:'uint8[]'},{name:'tokens',type:'address[]'},{name:'amountIn',type:'uint256'},{name:'extraDatas',type:'bytes[]'}],outputs:[{type:'uint256'}]}],
      functionName: 'quoteRoute',
      args: [
        ['0x4c4af8dbc524681930a27b2f1af5bcc8062e6fb7','0xba1333333333a1ba1108e8412f11850a5c319ba9'],
        [5,11],
        ['0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7','0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7','0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e'],
        5000n,
        ['0x', extraData]
      ]
    }),
    blockNumber: 80114945n
  });
  console.log('result:', result);
} catch(e: any) {
  console.log('error:', e.message?.slice(0, 300));
}
