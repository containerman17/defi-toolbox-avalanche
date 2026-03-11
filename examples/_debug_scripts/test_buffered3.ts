import { createPublicClient, http, encodeFunctionData, encodeAbiParameters } from 'viem';
import { avalanche } from 'viem/chains';

const client = createPublicClient({ chain: avalanche, transport: http('http://localhost:9650/ext/bc/C/rpc') });

// Test AUSD->USDC (known to work at 1.6 WAVAX amounts)
const extraData1 = encodeAbiParameters(
  [{type:'address'},{type:'address'},{type:'address'}],
  ['0x45cf39eeb437fa95bb9b52c0105254a6bd25d01e','0x31ae873544658654ce767bde179fd1bbcb84850b','0xe1bfc96d95badcb10ff013cb0c9c6c737ca07009']
);
console.log('Test 1: Direct AUSD->USDC (should work at 80104943)...');
try {
  const result = await client.call({
    to: '0x06380C0e0912312B5150364B9DC4542BA0DbBc85',
    data: encodeFunctionData({
      abi: [{name:'quoteRoute',type:'function',inputs:[{name:'pools',type:'address[]'},{name:'poolTypes',type:'uint8[]'},{name:'tokens',type:'address[]'},{name:'amountIn',type:'uint256'},{name:'extraDatas',type:'bytes[]'}],outputs:[{type:'uint256'}]}],
      functionName: 'quoteRoute',
      args: [
        ['0xba1333333333a1ba1108e8412f11850a5c319ba9'],
        [11],
        ['0x00000000efe302beaa2b3e6e1b18d08d69a9012a','0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e'],
        5000000000000000000n, // 5 AUSD
        [extraData1]
      ]
    }),
    blockNumber: 80104943n
  });
  console.log('SUCCESS! AUSD->USDC result:', result.data);
} catch(e: any) {
  console.log('FAIL:', e.message?.slice(0, 150));
}

// Test USDt->USDC via same pool
const extraData2 = encodeAbiParameters(
  [{type:'address'},{type:'address'},{type:'address'}],
  ['0x59933c571d200dc6a7fd1cda22495db442082e34','0x31ae873544658654ce767bde179fd1bbcb84850b','0xe1bfc96d95badcb10ff013cb0c9c6c737ca07009']
);
console.log('Test 2: Direct USDt->USDC via 0x31ae873...');
try {
  const result = await client.call({
    to: '0x06380C0e0912312B5150364B9DC4542BA0DbBc85',
    data: encodeFunctionData({
      abi: [{name:'quoteRoute',type:'function',inputs:[{name:'pools',type:'address[]'},{name:'poolTypes',type:'uint8[]'},{name:'tokens',type:'address[]'},{name:'amountIn',type:'uint256'},{name:'extraDatas',type:'bytes[]'}],outputs:[{type:'uint256'}]}],
      functionName: 'quoteRoute',
      args: [
        ['0xba1333333333a1ba1108e8412f11850a5c319ba9'],
        [11],
        ['0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7','0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e'],
        5000000n, // 5 USDt
        [extraData2]
      ]
    }),
    blockNumber: 80114945n
  });
  console.log('SUCCESS! USDt->USDC result:', result.data);
} catch(e: any) {
  console.log('FAIL:', e.message?.slice(0, 150));
}
