import { keccak256, toBytes } from "viem";

// Based on WooPP V2 source code, the function might be:
// swap(address baseToken, bool isSellingBase, uint256 amount, uint256 minAmount, address to, address rebateTo)
// or some variation
const sigs = [
  "swap(address,bool,uint256,uint256,address,address)",
  "swap(address,address,uint256,uint256)",
  "swap(address,address,uint256,uint256,address,address)",
  "swap(address,uint256,uint256,uint256,bytes32)",
  "swap(address,uint128,uint128,uint256,bytes)",
  "swap(address,int256,uint256,uint256,bytes)",
  "swap(address,uint256,uint256,uint128,bytes)",
  "sellBase(address,uint256,uint256,address,address)",
  "sellQuote(address,uint256,uint256,address,address)",
  "tryQuery(address,uint256,address)",
  "query(address,address,uint256)",
];
for (const sig of sigs) {
  const h = keccak256(toBytes(sig)).slice(0, 10);
  console.log(sig + " -> " + h);
  if (h === "0xac8bb7d9") console.log("  *** MATCH ***");
}
