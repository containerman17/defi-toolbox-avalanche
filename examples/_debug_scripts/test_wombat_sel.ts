import { keccak256 } from "viem";

const sigs = [
  "swap(address,address,uint256,uint256,address,uint256)",
  "swap(address,address,uint256,uint256,address)",
  "swap(address,address,uint256,address,uint256)",
  "swapFromV1(address,address,uint256,uint256,address)",
  "deposit(address,uint256,address)",
  "quotePotentialSwap(address,address,uint256)",
];

for (const sig of sigs) {
  const sel = keccak256(Buffer.from(sig)).slice(0, 10);
  console.log(`${sig} => ${sel}`);
}
