import { keccak256, toBytes } from "viem";
const target = "0xac8bb7d9";
const sigs = [
  "swap(address,bool,uint256,uint256,address,address)",
  "swap(address,int256,int256,uint160,address,bytes)",
  "swap(address,bool,uint256,uint128,address)",
  "swap(address,uint256,uint256,uint128,bytes)",
  "swap(address,uint256,uint256,uint256,bytes)",
  "swap(address,bool,uint256,uint128,address,address)",
  "swap(address,bool,int256,uint160,address,bytes)",
  "buyBaseToken(uint256,uint256,bytes)",
  "sellBaseToken(uint256,uint256,bytes)",
  "swap(address,uint128,uint128,address,address)",
  "swap(address,bool,uint128,uint128,address,address)",
  "swap(address,bool,uint256,uint256,uint256,bytes)",
  "swap(address,uint256,uint256,uint256,bytes,address)",
];
for (const sig of sigs) {
  const h = keccak256(toBytes(sig)).slice(0, 10);
  if (h === target) console.log("MATCH:", sig);
}
console.log("done");
