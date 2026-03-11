import { keccak256 } from "viem";

const sigs = [
  "query(address,address,uint256)",
  "tryQuery(address,address,uint256)",
  "swap(address,uint256,uint256,uint256,bytes)",
  "swap(address,address,address,uint256,uint256,address)",
];
for (const sig of sigs) {
  const sel = keccak256(Buffer.from(sig)).slice(0, 10);
  console.log(`${sig}  =>  ${sel}`);
}
