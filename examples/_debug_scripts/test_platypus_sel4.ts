import { keccak256 } from "viem";

// Maybe 4 params not 5, with different types
const sigs = [
  "swap(uint256,uint256,uint256,address)",
  "swap(uint128,uint256,uint256,address)",
  "swap(uint256,uint128,uint256,address)",
  "swapForExact(uint256,uint256,uint256,address)",
  "exchange(uint256,uint256,uint256,address)",
  // Or maybe it's address-based with different encoding
  "swap(address,uint256,address,uint256)",
];

for (const sig of sigs) {
  const sel = keccak256(Buffer.from(sig)).slice(0, 10);
  if (sel === "0x91695586") console.log(`MATCH: ${sig}`);
  else console.log(`${sig} => ${sel}`);
}

// Let me try another approach - look at the Platypus Exchange source code
// Platypus has pools with this interface:
// Pool.swap(address fromToken, address toToken, uint256 fromAmount, uint256 minimumToAmount, address to, uint256 deadline)
// selector = 9908fc8b

// But this one uses integers... could be Platypus v2 or Wombat v2

// Let's look at what functions the implementation 0x84a420459cd31c3c34583f67e0f0fb191067d32f exports
console.log("\n0x91695586 - let's verify with known platypus swaps");
