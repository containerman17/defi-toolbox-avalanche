import { keccak256 } from "viem";

// 0xfe029156 - we know: swap(address fromToken, address toToken, uint256 amount) - 3 params
// Let's check more signatures
const sigs = [
  "swap(address,address,uint256)",
  "swap(address,address,uint256,address)",
  "swap(address,address,uint256,uint256)",
  "swap(address,address,uint256,address,uint256)",
  "sellBase(address,uint256,address)",
  "swap(address,uint256,uint256,bool)",
  "exchange(address,address,uint256)",
  "exactInput(address,address,uint256)",
  "swapExact(address,address,uint256,address)",
];

for (const sig of sigs) {
  const sel = keccak256(Buffer.from(sig)).slice(0, 10);
  console.log(`${sig} => ${sel}`);
  if (sel === "0xfe029156") console.log("  *** MATCH ***");
}
