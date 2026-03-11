import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const block = BigInt(80108339) - 1n;

const code = await client.getBytecode({ address: WOOPP as `0x${string}`, blockNumber: block });
if (!code) { console.log("No code"); process.exit(1); }

// Extract printable strings
const hex = code.slice(2);
const bytes = Buffer.from(hex, "hex");
const strings: string[] = [];
let current = "";
for (const b of bytes) {
  if (b >= 32 && b < 127) {
    current += String.fromCharCode(b);
  } else {
    if (current.length >= 5) strings.push(current);
    current = "";
  }
}

console.log("Readable strings:");
for (const s of strings) {
  if (s.match(/[a-zA-Z ]{4,}/)) {
    console.log(" ", JSON.stringify(s));
  }
}
