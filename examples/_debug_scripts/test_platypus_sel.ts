import { keccak256 } from "viem";

const sigs = [
  // Platypus-style
  "swap(uint8,uint8,uint256,uint256,address)",
  "swap(uint256,uint256,uint256,uint256,address)",
  "swapTokensForTokens(address,address,uint256,uint256,address,uint256)",
  // wombat
  "swap(address,address,uint256,uint256,address,uint256)",
  "swapCredit(address,address,uint256,uint256,address,uint256)",
  // Other
  "swap(uint8,uint8,uint256,uint256)",
  "swap(uint256,uint8,uint8,uint256,address)",
  "exactInput(address,address,uint256,uint256,address)",
];

for (const sig of sigs) {
  const sel = keccak256(Buffer.from(sig)).slice(0, 10);
  if (sel === "0x91695586") console.log(`MATCH: ${sig}`);
  else console.log(`${sig} => ${sel}`);
}

// Also just print 0x91695586
console.log("Target: 0x91695586");

// Decode the actual calldata
const calldata = "0x91695586000000000000000000000000000000000000000000000000000000000000000200000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000002044a8200000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000";
// 4 bytes selector + 5 * 32 bytes
console.log("Params:");
for (let i = 0; i < 5; i++) {
  const start = 10 + i * 64;
  const end = start + 64;
  console.log(`  param${i}: 0x${calldata.slice(start, end)} = ${BigInt("0x" + calldata.slice(start, end))}`);
}
