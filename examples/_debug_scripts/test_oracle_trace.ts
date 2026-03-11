import { createPublicClient, http, decodeAbiParameters } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const ORACLE = "0xd92e3c8f1c5e835e4e76173c4e83bf517f61b737";
const ORACLE_SUB = "0x5ecf662ae95afbb01c80fbfb6efce3ae1e770df7";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

// Test the oracle sub-contract directly
// b5e4b813 with args e3faa9d01a7e... (WAVAX hash or token ID?)
// This might be a Chainlink aggregator selector

// Try latestRoundData() = 0xfeaf968c
const lrd = await client.request({
  method: "eth_call" as any,
  params: [{ to: ORACLE_SUB, data: "0xfeaf968c" }, blockHex] as any,
}).catch(e => `err: ${e.message?.slice(0,100)}`);
console.log("latestRoundData():", lrd);

// Try decimals() = 0x313ce567
const dec = await client.request({
  method: "eth_call" as any,
  params: [{ to: ORACLE_SUB, data: "0x313ce567" }, blockHex] as any,
}).catch(e => `err: ${e.message?.slice(0,100)}`);
console.log("decimals():", dec);

// Check code at oracle sub
const code = await client.request({
  method: "eth_getCode" as any,
  params: [ORACLE_SUB, blockHex] as any,
});
console.log("Oracle sub code length:", ((code as string).length - 2) / 2, "bytes");

// The b5e4b813 function with args:
// e3faa9d01a7e10bfc3432f0aa1f39e8f3a3da3e38c6fe3ffb8b6ccbd12c9f3fb (hash of WAVAX/USD?)
// Let's just try calling with full args
const raw = "0xb5e4b813e3faa9d01a7e10bfc3432f0aa1f39e8f3a3da3e38c6fe3ffb8b6ccbd12c9f3fb0000000000000000000000000000000000000000000000000000000000000000";
const r = await client.request({
  method: "eth_call" as any,
  params: [{ to: ORACLE_SUB, data: raw }, blockHex] as any,
}).catch(e => `err: ${e.message?.slice(0,100)}`);
console.log("b5e4b813(arg1,arg2):", r);

// Debug trace of oracle call
const trace = await client.request({
  method: "debug_traceCall" as any,
  params: [
    { to: ORACLE, data: "0xc1701b67000000000000000000000000b31f66aa3c1e785363f0875a1b74e27b85fd66c7" },
    blockHex,
    { tracer: "callTracer" }
  ] as any,
}) as any;
console.log("\nOracle trace:");
function printTrace(call: any, depth = 0) {
  const indent = "  ".repeat(depth);
  console.log(`${indent}${call.type} ${(call.from||"").slice(0,10)}→${(call.to||"").slice(0,10)} ${(call.input||"").slice(0,20)} out=${(call.output||"").slice(0,66)} ${call.error||""}`);
  for (const sub of (call.calls||[])) printTrace(sub, depth+1);
}
printTrace(trace);
