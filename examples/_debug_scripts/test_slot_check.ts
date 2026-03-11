import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const ORACLE = "0xd92e3c8f1c5e835e4e76173c4e83bf517f61b737";
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

// Check actual storage values at this block
for (const slot of ["0x0", "0x1", "0x5", "0x7"]) {
  const val = await client.getStorageAt({ address: WOOPP as `0x${string}`, slot: slot as `0x${string}`, blockNumber: block });
  console.log(`WooPP[${slot}] = ${val}`);
}

for (const slot of ["0x1", "0x4", "0x5", "0x6", "0x7"]) {
  const val = await client.getStorageAt({ address: ORACLE as `0x${string}`, slot: slot as `0x${string}`, blockNumber: block });
  console.log(`Oracle[${slot}] = ${val}`);
}

// Also check if node has this block
const bn = await client.getBlockNumber();
console.log("Current block:", bn);
const targetBlock = await client.getBlock({ blockNumber: block }).catch(e => null);
console.log("Target block exists:", targetBlock != null);
