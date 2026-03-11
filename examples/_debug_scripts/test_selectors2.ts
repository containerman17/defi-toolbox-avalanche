import { keccak256 } from "viem";

// The Solidity says selector 0xac8bb7d9 for:
// swap(address broker, uint256 direction, uint256 amount, uint256 minOutput, bytes data)
// Let's verify
const sigs = [
  "swap(address,uint256,uint256,uint256,bytes)",
  "sellBase(address,uint256,address)",
  "sellQuote(address,uint256,address)",
  "inCaseTokenGotStuck(address,address)",
  "deposit(address,uint256)",
  "query(address,address,uint256)",
];
for (const sig of sigs) {
  const sel = keccak256(Buffer.from(sig)).slice(0, 10);
  console.log(`${sig}  =>  ${sel}`);
}

// Also check what 0xac8bb7d9 might be
// Let's try a different approach - check on-chain for the WooPP V2 pool abi
console.log("\nExpected: 0xac8bb7d9");
