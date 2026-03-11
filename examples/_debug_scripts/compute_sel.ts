import { keccak256, toHex } from "viem";
const sigs = [
  'swap(address,address,uint256,uint256,address,address)',
  'swap(address,address,uint256,uint256,address)',
  'tryQuerySwap(address,address,uint256)',
  'querySwap(address,address,uint256)',
];
for (const sig of sigs) {
  const sel = keccak256(toHex(sig)).slice(0,10);
  console.log(sel, sig);
}
