import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

// Get bytecode to find function selectors
const code = await client.getBytecode({ address: WOOPP as `0x${string}`, blockNumber: block });
if (!code) { console.log("No code!"); process.exit(1); }
console.log("Code length:", code.length);
console.log("Code starts with:", code.slice(0, 20));

// WooPP V2 source has these functions: 
// ac8bb7d9 = swap(address,uint256,uint256,uint256,bytes)
// Let me find selectors in bytecode by looking for JUMPI patterns
// Actually let me just scan for 4-byte patterns at JUMPI locations
// A simpler approach: the contract is a proxy, check what it delegates to
// EIP-1967 implementation slot: 0x360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc
const implSlot = await client.getStorageAt({
  address: WOOPP as `0x${string}`,
  slot: "0x360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc" as `0x${string}`,
  blockNumber: block,
});
console.log("EIP-1967 impl slot:", implSlot);

// Try EIP-897 slot
const slot0 = await client.getStorageAt({
  address: WOOPP as `0x${string}`,
  slot: "0x0000000000000000000000000000000000000000000000000000000000000000" as `0x${string}`,
  blockNumber: block,
});
console.log("Slot 0:", slot0);

// Is this contract verified? Let's check what format slot 5 is
// slot 5: 000069b11cb40186a00186a0ef3690cbb1217410833f093ce56f7a1603787cad
// This looks like an address (0xef3690cbb1217410833f093ce56f7a1603787cad packed with params)
