import { keccak256, toBytes } from "viem";
const sigs = [
  "swap(address,uint256,uint256,uint256,bytes)",
  "swap(address,bool,uint256,uint256,bytes)",
  "swap(address,uint8,uint256,uint256,bytes)",
  "tryQuery(address,address,uint256)",
];
for (const sig of sigs) {
  const h = keccak256(toBytes(sig)).slice(0, 10);
  console.log(sig + " -> " + h);
}
