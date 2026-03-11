import { keccak256, toBytes } from "viem";
const target = "0xac8bb7d9";

// More systematic: try variations of pool swap functions
const patterns = [
  // From WooFi V2 codebase based on pools
  "swap(address,bool,uint256,uint256,bytes,uint256)",
  "swap(address,bool,uint256,uint256,bytes)",
  "swap(address,bool,uint128,uint128,bytes)",
  "swap(address,bool,uint256,uint128,bytes)",
  "sell_base(address,uint256,uint256,bytes)",
  "sell_quote(address,uint256,uint256,bytes)",
  "swap(address,uint256,bool,uint256,bytes)",
  // WooPP V2 might have specific signature
  "swap(address,uint256,uint256,uint256,address)",
  "poolSwap(address,bool,uint256,uint256,bytes)",
];
for (const sig of patterns) {
  const h = keccak256(toBytes(sig)).slice(0, 10);
  if (h === target) console.log("MATCH:", sig);
  else console.log(sig, "->", h);
}
