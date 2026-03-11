import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const block = BigInt(80108339) - 1n;

const code = await client.getBytecode({ address: WOOPP as `0x${string}`, blockNumber: block });
if (!code) { console.log("No code"); process.exit(1); }

// Extract ASCII strings from bytecode  
const hex = code.slice(2);
const ascii: string[] = [];
let current = "";
for (let i = 0; i < hex.length - 1; i += 2) {
  const byte = parseInt(hex.slice(i, i+2), 16);
  if (byte >= 32 && byte <= 126) {
    current += String.fromCharCode(byte);
  } else {
    if (current.length >= 4) ascii.push(current);
    current = "";
  }
}
if (current.length >= 4) ascii.push(current);

console.log("ASCII strings in WooPP bytecode:");
for (const s of ascii.slice(0, 50)) {
  console.log(" ", s);
}
