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

// Print all addresses with "5ecf" in them
for (const [addr, data] of Object.entries(prestate) as any) {
  if (addr.includes("5ecf")) {
    console.log(`Address ${addr}:`, JSON.stringify(data.storage, null, 2));
  }
}

// Check oracle 0xd92e3c8f
for (const [addr, data] of Object.entries(prestate) as any) {
  if (addr.includes("d92e3c8f")) {
    console.log(`Address ${addr}:`, JSON.stringify(data.storage, null, 2));
  }
}

// Check aba7ed 
for (const [addr, data] of Object.entries(prestate) as any) {
  if (addr.includes("aba7ed")) {
    console.log(`Address ${addr}:`, JSON.stringify(data.storage, null, 2));
  }
}
