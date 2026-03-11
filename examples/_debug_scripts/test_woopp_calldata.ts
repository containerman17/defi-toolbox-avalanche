// Decode exact calldata from real WooPP swap  
import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";

// Real calldata:
const realInput = "0xac8bb7d900000000000000000000000063242a4ea82847b20e506b63b0e2e2eff0cc6cb0000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000001270b018000000000000000000000000000000ffffffffffffffffffffffffffffffff00000000000000000000000000000000000000000000000000000000000000a00000000000000000000000000000000000000000000000000000000000000020000000000000000000000000b97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";

// Decode it
const selector = realInput.slice(0, 10);
const argsHex = realInput.slice(10);

// Split into 32-byte words
const words = [];
for (let i = 0; i < argsHex.length; i += 64) {
  words.push(argsHex.slice(i, i + 64));
}

console.log("Selector:", selector);
for (let i = 0; i < words.length; i++) {
  console.log(`  Word ${i}: 0x${words[i]}`);
}

// arg3 = amount: 
const amount = BigInt("0x" + words[2]);
console.log("Amount:", amount);

// arg4 = minOutput:
const minOutput = BigInt("0x" + words[3]);
console.log("minOutput (raw):", "0x" + words[3]);
console.log("minOutput as uint256:", minOutput);
console.log("minOutput lower 128 bits:", minOutput & ((1n << 128n) - 1n));
console.log("minOutput upper 128 bits:", minOutput >> 128n);

// Our calldata:
// abi.encode(address, direction, amountIn, 0)
// = 32+32+32+32 = 128 bytes
// Then 0xa0 (offset), 0x20 (length), baseToken = another 96 bytes
// So total args = 224 bytes, full calldata = 228 bytes

// What does our Solidity encode produce for minOutput?
// abi.encode(address(this), isSellingBase?1:0, amountIn, uint256(0)) 
// = [addr, 0, 4950000000, 0] -> word3 = 0x0000000000000000000000000000000000000000000000000000000000000000

console.log("\nOur minOutput: 0x" + "0".padStart(64, "0"));
console.log("Real minOutput: " + words[3]);
