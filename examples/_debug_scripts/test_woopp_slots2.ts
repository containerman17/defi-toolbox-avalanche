import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const ORACLE_ADDR = "0xd92e3c8f60a91fa6dfc59a43f7e1f7e43ee56be4";
const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const FEED = "0x5ecf662ae95afbb01c80fbfb6efce3ae1e770df7";

const txHash = "0x1f84c56947f5d4e8d12096ea6c4622f368405b87f96f0924d8775453cea4bbff";

const prestateTrace = await client.request({
  method: "debug_traceTransaction" as any,
  params: [txHash, { tracer: "prestateTracer", tracerConfig: { diffMode: false } }] as any,
});

const prestate = prestateTrace as any;

// Print all addresses accessed
console.log("All accessed addresses:");
for (const [addr, data] of Object.entries(prestate) as any) {
  const storage = data.storage || {};
  const slots = Object.keys(storage);
  if (slots.length > 0) {
    console.log(`  ${addr}: ${slots.length} slots`);
  }
}

// Print oracle storage
const oracleData = prestate[ORACLE_ADDR.toLowerCase()];
if (oracleData) {
  console.log("\nOracle storage:", JSON.stringify(oracleData.storage, null, 2));
} else {
  console.log("\nOracle not found in prestate");
  // Try with checksummed addr
  for (const [k, v] of Object.entries(prestate)) {
    if (k.toLowerCase() === ORACLE_ADDR.toLowerCase()) {
      console.log("Found oracle at", k, ":", JSON.stringify((v as any).storage, null, 2));
    }
  }
}

// Print feed storage
const feedData = prestate[FEED.toLowerCase()];
if (feedData) {
  console.log("\nFeed storage:", JSON.stringify(feedData.storage, null, 2));
} else {
  console.log("\nFeed not found in prestate");
}
