// Use the correct block
import { createPublicClient, http, decodeAbiParameters, type Hex } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const WOOPP = "0xABa7eD514217D51630053d73D358aC2502d3f9BB";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";

// The real block that the tx happened in
const realBlock = 80108339n;
const blockHex = `0x${(realBlock - 1n).toString(16)}`;

// Use "latest" to check if pool is still functional
// First let's check that the pool isn't paused
const pauseData = "0xb187bd26"; // isPaused()
try {
  const pauseResult = await client.request({
    method: "eth_call" as any,
    params: [{ to: WOOPP, data: pauseData }, "latest"] as any,
  });
  console.log("isPaused (latest):", pauseResult);
} catch (e: any) {
  console.log("isPaused failed:", e.message?.slice(0,100));
}

// Try at the specific block
try {
  const pauseResult = await client.request({
    method: "eth_call" as any,
    params: [{ to: WOOPP, data: pauseData }, blockHex] as any,
  });
  console.log(`isPaused at ${blockHex}:`, pauseResult);
} catch (e: any) {
  console.log(`isPaused at block failed:`, e.message?.slice(0,100));
}

// Try getPrice(USDC) to see if oracle is responsive
const getPriceData = "0x41976e09" + USDC.slice(2).padStart(64, "0");
try {
  const priceResult = await client.request({
    method: "eth_call" as any,
    params: [{ to: WOOPP, data: getPriceData }, blockHex] as any,
  });
  const [price] = decodeAbiParameters([{type:"uint256"}], priceResult as `0x${string}`);
  console.log("USDC price at block:", price);
} catch (e: any) {
  console.log("getPrice failed:", e.message?.slice(0,100));
}

// Check the oracle contract
const getOracleData = "0x7dc0d1d0"; // oracle()
try {
  const oracleResult = await client.request({
    method: "eth_call" as any,
    params: [{ to: WOOPP, data: getOracleData }, blockHex] as any,
  });
  console.log("oracle address:", oracleResult);
} catch (e: any) {
  console.log("oracle() failed:", e.message?.slice(0,100));
}
