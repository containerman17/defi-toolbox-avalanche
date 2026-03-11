// Find WooPP oracle storage slots using debug_storageRangeAt
import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

// The oracle address from traces
const ORACLE_ADDR = "0xd92e3c8f60a91fa6dfc59a43f7e1f7e43ee56be4";
const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const block = BigInt(80108339) - 1n;

// Get tx from the real transaction to see which storage slots get accessed
const txHash = "0x1f84c56947f5d4e8d12096ea6c4622f368405b87f96f0924d8775453cea4bbff";

// Use debug_traceTransaction with prestate tracer to get storage
const prestateTrace = await client.request({
  method: "debug_traceTransaction" as any,
  params: [
    txHash,
    { 
      tracer: "prestateTracer",
      tracerConfig: { diffMode: false }
    }
  ] as any,
});

const prestate = prestateTrace as any;

// Check WooPP pool storage  
if (prestate[WOOPP.toLowerCase()]) {
  const storage = prestate[WOOPP.toLowerCase()].storage || {};
  const slots = Object.keys(storage);
  console.log(`WooPP pool has ${slots.length} storage slots accessed:`);
  for (const [slot, val] of Object.entries(storage).slice(0, 20)) {
    console.log(`  ${slot} = ${val}`);
  }
}

// Check oracle storage
if (prestate[ORACLE_ADDR.toLowerCase()]) {
  const storage = prestate[ORACLE_ADDR.toLowerCase()].storage || {};
  const slots = Object.keys(storage);
  console.log(`\nOracle has ${slots.length} storage slots accessed:`);
  for (const [slot, val] of Object.entries(storage).slice(0, 20)) {
    console.log(`  ${slot} = ${val}`);
  }
}

// Also check 0x5ecf662a
const FEED = "0x5ecf662ae95afbb01c80fbfb6efce3ae1e770df7";
if (prestate[FEED.toLowerCase()]) {
  const storage = prestate[FEED.toLowerCase()].storage || {};
  console.log(`\nFeed has ${Object.keys(storage).length} storage slots accessed:`);
  for (const [slot, val] of Object.entries(storage).slice(0, 20)) {
    console.log(`  ${slot} = ${val}`);
  }
}
