import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const ORACLE = "0xd92e3c8f1c5e835e4e76173c4e83bf517f61b737";
const FEED = "0x5ecf662abb8c2ab099862f9ef2ddc16cbc8a9977";
const block = BigInt(80108339) - 1n;

const oracleCode = await client.getBytecode({ address: ORACLE as `0x${string}`, blockNumber: block });
const feedCode = await client.getBytecode({ address: FEED as `0x${string}`, blockNumber: block });

console.log("Oracle code length:", oracleCode?.length ?? 0);
console.log("Feed code length:", feedCode?.length ?? 0);

// Try calling oracle's c1701b67 function directly
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const blockHex = `0x${block.toString(16)}`;

const result = await client.request({
  method: "eth_call" as any,
  params: [
    { to: ORACLE, data: "0xc1701b67" + WAVAX.slice(2).padStart(64, "0") },
    blockHex,
  ] as any,
}).catch(e => `err: ${e.message?.slice(0, 100)}`);
console.log("oracle.c1701b67(WAVAX):", result);

// Try calling FEED directly with b5e4b813
const feedResult = await client.request({
  method: "eth_call" as any,
  params: [
    { to: FEED, data: "0xb5e4b813" + WAVAX.slice(2).padStart(64, "0") },
    blockHex,
  ] as any,
}).catch(e => `err: ${e.message?.slice(0, 100)}`);
console.log("feed.b5e4b813(WAVAX):", feedResult);

// Try debug_traceCall on oracle
const trace = await client.request({
  method: "debug_traceCall" as any,
  params: [
    { to: ORACLE, data: "0xc1701b67" + WAVAX.slice(2).padStart(64, "0") },
    blockHex,
    { tracer: "callTracer" },
  ] as any,
}) as any;
console.log("\noracle trace:");
function printTrace(call: any, depth = 0) {
  const indent = "  ".repeat(depth);
  console.log(`${indent}${call.type} ${(call.to||"").slice(0,10)} ${(call.input||"").slice(0,10)} out=${(call.output||"").slice(0,34)} ${call.error||""}`);
  for (const sub of (call.calls||[])) printTrace(sub, depth+1);
}
printTrace(trace);
