import { keccak256 } from "viem";

// params: 2, 0, 33835650, 0, <address>
// These look like token indices! This is Platypus-style with index-based tokens
// or Curve-style exchange(i, j, dx, min_dy)

const sigs = [
  // Platypus with indices
  "swap(uint256,uint256,uint256,uint256,address)",
  "exchange(int128,int128,uint256,uint256)",
  "exchange(uint256,uint256,uint256,uint256)",
  "exchange_underlying(int128,int128,uint256,uint256)",
  "swapTo(uint256,uint256,uint256,uint256,address)",
  "swapExactTokensForTokens(uint256,uint256,uint256,uint256,address)",
  // The actual params are (uint, uint, uint, uint, addr) -> 5 params
  "swap(uint256,uint256,uint256,uint256,address)",
];

for (const sig of sigs) {
  const sel = keccak256(Buffer.from(sig)).slice(0, 10);
  if (sel === "0x91695586") console.log(`MATCH: ${sig}`);
  else console.log(`${sig} => ${sel}`);
}
