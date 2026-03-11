import { keccak256 } from "viem";

const sig = "swap(uint8,uint8,uint256,uint256,uint256)";
const sel = keccak256(Buffer.from(sig)).slice(0, 10);
console.log(`${sig} => ${sel}`);

// And calculateSwap:
const sig2 = "calculateSwap(uint8,uint8,uint256)";
const sel2 = keccak256(Buffer.from(sig2)).slice(0, 10);
console.log(`${sig2} => ${sel2}`);
