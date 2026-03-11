import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const SUB = "0x5ecf662ae95afbb01c80fbfb6efce3ae1e770df7";
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

for (const [label, bHex] of [
  ["block-1", `0x${(BigInt(80108339)-1n).toString(16)}`],
  ["block", `0x${BigInt(80108339).toString(16)}`],
  ["latest", "latest"],
]) {
  const code = await client.request({
    method: "eth_getCode" as any,
    params: [SUB, bHex] as any,
  });
  console.log(`Code at ${label}: ${((code as string).length - 2) / 2} bytes`);
}

// Verify by calling the function directly
const data = "0xb5e4b813" + "e3faa9d01a7e10bfc3432f0aa1f39e8f3a3da3e38c6fe3ffb8b6ccbd12c9f3fb";
try {
  const r = await client.request({
    method: "eth_call" as any,
    params: [{ to: SUB, data }, blockHex] as any,
  });
  console.log("Direct call result:", r?.slice(0, 66));
} catch (e: any) {
  console.log("Direct call error:", e.message?.slice(0, 100));
}

// Check if this is a proxy or has a special structure
const slot0 = await client.request({
  method: "eth_getStorageAt" as any,
  params: [SUB, "0x0", blockHex] as any,
});
console.log("Storage slot 0:", slot0);
