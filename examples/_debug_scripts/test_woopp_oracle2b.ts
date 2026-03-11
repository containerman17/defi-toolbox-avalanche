import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const ORACLE = "0xd92e3c8f1c5e835e4e76173c4e83bf517f61b737"; // Full address
const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";

const block = BigInt(80108339);
const blockHex = `0x${(block-1n).toString(16)}`;

// The oracle function called was 0xc1701b67(WAVAX)
const oracleData = "0xc1701b67000000000000000000000000b31f66aa3c1e785363f0875a1b74e27b85fd66c7";

for (const [label, bHex] of [
  ["block-1", `0x${(block-1n).toString(16)}`],
  ["block", `0x${block.toString(16)}`],
  ["latest", "latest"],
]) {
  try {
    const result = await client.request({
      method: "eth_call" as any,
      params: [{ to: ORACLE, data: oracleData, from: WOOPP }, bHex] as any,
    });
    console.log(`Oracle c1701b67(WAVAX) at ${label}: ${result}`);
  } catch (e: any) {
    console.log(`Oracle at ${label}: revert - ${e.message?.slice(0, 80)}`);
  }
}

// Also check WooPP tryQuery
const tryQueryData = "0x7dc2038a" + 
  USDC.slice(2).padStart(64, "0") +
  WAVAX.slice(2).padStart(64, "0") +
  (4950000000n).toString(16).padStart(64, "0");

for (const [label, bHex] of [
  ["block-1", `0x${(block-1n).toString(16)}`],
  ["block", `0x${block.toString(16)}`],
]) {
  try {
    const result = await client.request({
      method: "eth_call" as any,
      params: [{ to: WOOPP, data: tryQueryData }, bHex] as any,
    });
    console.log(`WooPP tryQuery at ${label}: ${result}`);
  } catch (e: any) {
    console.log(`WooPP tryQuery at ${label}: revert - ${e.message?.slice(0, 80)}`);
  }
}
