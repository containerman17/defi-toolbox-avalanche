import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const txHash = "0x1f84c56947f5d4e8d12096ea6c4622f368405b87f96f0924d8775453cea4bbff";

const prestateTrace = await client.request({
  method: "debug_traceTransaction" as any,
  params: [txHash, { tracer: "prestateTracer", tracerConfig: { diffMode: false } }] as any,
});

const prestate = prestateTrace as any;

// Print WOOPP storage
const woopp = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
console.log("WooPP storage:", JSON.stringify(prestate[woopp]?.storage || prestate[woopp.toLowerCase()]?.storage, null, 2));

// Print oracle
const oracle = "0xd92e3c8f1c5e835e4e76173c4e83bf517f61b737";
console.log("\nOracle storage (d92e3c8f1c5e...):", JSON.stringify(prestate[oracle]?.storage || {}, null, 2));
