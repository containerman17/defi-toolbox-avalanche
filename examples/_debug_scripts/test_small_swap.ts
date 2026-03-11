import { createPublicClient, http, encodeFunctionData, encodeAbiParameters } from 'viem';
import { avalanche } from 'viem/chains';

const client = createPublicClient({ chain: avalanche, transport: http('http://localhost:9650/ext/bc/C/rpc') });

// Test 1: Direct USDt -> USDC via BalV3 buffered (using waAvaUSDT + waAvaUSDC pool 0x31ae873)
const extraData1 = encodeAbiParameters(
  [{type:'address'},{type:'address'},{type:'address'}],
  ['0x59933c571d200dc6a7fd1cda22495db442082e34','0x31ae873544658654ce767bde179fd1bbcb84850b','0xe1bfc96d95badcb10ff013cb0c9c6c737ca07009']
);
console.log('Testing direct USDt->USDC via 0x31ae873 (waAvaUSDT/waAvaUSDC pool)...');
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
        5000n,
        [extraData1]
      ]
    }),
    blockNumber: 80114945n
  });
  console.log('SUCCESS! USDt->USDC result:', result.data);
} catch(e: any) {
  console.log('FAIL:', e.message?.slice(0, 200));
}

// Test 2: WooFi USDt->WAVAX then BalV3 buffered WAVAX->USDC (current route)
const extraData2 = encodeAbiParameters(
  [{type:'address'},{type:'address'},{type:'address'}],
  ['0xd7da0de6ef4f51d6206bf2a35fcd2030f54c3f7b','0xa4e1b0ddffc0e3aa63dbca462cf370e4f1dc9b8b','0xe1bfc96d95badcb10ff013cb0c9c6c737ca07009']
);
console.log('Testing WooFi USDt->WAVAX then buffered WAVAX->USDC...');
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
        ['0x', extraData2]
      ]
    }),
    blockNumber: 80114945n
  });
  console.log('SUCCESS! WooFi+Buffered result:', result.data);
} catch(e: any) {
  console.log('FAIL:', e.message?.slice(0, 200));
}
