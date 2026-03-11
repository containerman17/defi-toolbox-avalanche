// Check oracle sub-contract at 0x5ecf662a
import { createPublicClient, http, decodeAbiParameters } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const ORACLE_MAIN = "0xd92e3c8f1c5e835e4e76173c4e83bf517f61b737";
const ORACLE_SUB = "0x5ecf662ae95afbb01c80fbfb6efce3ae1e770df7";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

// b5e4b813 with WAVAX e3faa9d01a...
// This is likely latestRoundData() or a similar function
// From the trace: STATICCALL 0xd92e3c8f→0x5ecf662a 0xb5e4b813e3faa9d01a
// Let me check what this address is
console.log("Checking oracle sub-contracts...");

// feeder() or underlying oracle
const feederData = "0xb5e4b813";
const wavaxArg = "e3faa9d01a7e10bfc3432f0aa1f39e8f3a3da3e38c6fe3ffb8b6ccbd12c9f3fb"; // some param from trace

// Try with raw input from trace
const raw1 = "0xb5e4b813" + "e3faa9d01a7e10bfc3432f0aa1f39e8f3a3da3e38c6fe3ffb8b6ccbd12c9f3fb";

try {
  const r1 = await client.request({
    method: "eth_call" as any,
    params: [{ to: ORACLE_SUB, data: raw1 }, blockHex] as any,
  });
  console.log("Oracle sub b5e4b813(arg1):", r1);
} catch (e: any) {
  console.log("b5e4b813(arg1) revert:", e.message?.slice(0, 80));
}

// The second call used 0x3f64001122... as the second argument
const raw2 = "0xb5e4b813" + "3f64001122d13e3b90b35e32e5e4b3f00e02e1b7";

// Also try: 893ddf87 function
const raw3 = "0x893ddf87" + "e3faa9d01a7e10bfc3432f0aa1f39e8f3a3da3e38c6fe3ffb8b6ccbd12c9f3fb";
try {
  const r3 = await client.request({
    method: "eth_call" as any,
    params: [{ to: ORACLE_SUB, data: raw3 }, blockHex] as any,
  });
  console.log("Oracle sub 893ddf87(arg1):", r3);
} catch (e: any) {
  console.log("893ddf87(arg1) revert:", e.message?.slice(0, 80));
}

// Try calling the oracle main directly with WAVAX
const mainData = "0xc1701b67000000000000000000000000b31f66aa3c1e785363f0875a1b74e27b85fd66c7";
try {
  const r = await client.request({
    method: "eth_call" as any,
    params: [{ to: ORACLE_MAIN, data: mainData }, blockHex] as any,
  });
  const [price, spread] = decodeAbiParameters([{type:"uint256"},{type:"uint256"}], r as `0x${string}`);
  console.log("Oracle price(WAVAX):", price, "spread:", spread);
} catch (e: any) {
  console.log("oracle c1701b67 revert:", e.message?.slice(0, 80));
}
