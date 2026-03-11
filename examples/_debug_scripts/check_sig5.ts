import { keccak256, toBytes } from "viem";
const sigs = [
  "_executeSingle(address,uint8,address,address,uint256,bytes)",
  "_executeSingleRevert(address,uint8,address,address,uint256,bytes)",
];
for (const sig of sigs) {
  console.log(sig, "->", keccak256(toBytes(sig)).slice(0, 10));
}
