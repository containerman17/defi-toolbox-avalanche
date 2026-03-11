import { keccak256 } from "viem";

// Params: index0=2, index1=0, amount=33835650, minOut=0, addr=?
// This looks like Platypus Exchange's swap function
// Platypus: swapTokensForTokens(fromToken, toToken, fromAmount, minimumToAmount, to, deadline)
// But we have integers for first 2 args (2 and 0), suggesting indices

// Let's check Platypus Pool functions:
const sigs = [
  "exchange(uint256,uint256,uint256,uint256,address)",
  "swap(uint128,uint128,uint256,uint256,address)",
  "swap(uint64,uint64,uint256,uint256,address)",
  "swap(uint32,uint32,uint256,uint256,address)",
  "swap(uint16,uint16,uint256,uint256,address)",
  "swap(uint8,uint16,uint256,uint256,address)",
  "swap(uint16,uint8,uint256,uint256,address)",
  "swap(int128,int128,uint256,uint256,address)",
  "exchange(int256,int256,uint256,uint256,address)",
  "swap(uint256,uint256,uint128,uint128,address)",
  "exchange(uint128,uint128,uint256,uint256,address)",
];

for (const sig of sigs) {
  const sel = keccak256(Buffer.from(sig)).slice(0, 10);
  if (sel === "0x91695586") console.log(`MATCH: ${sig}`);
  else console.log(`${sig} => ${sel}`);
}
