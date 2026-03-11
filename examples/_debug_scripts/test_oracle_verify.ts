import { createPublicClient, http, decodeAbiParameters } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const SUB = "0x5ecf662ae95afbb01c80fbfb6efce3ae1e770df7";
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

// Let's call the oracle MAIN directly (it works) and see what it returns
const ORACLE_MAIN = "0xd92e3c8f1c5e835e4e76173c4e83bf517f61b737";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const mainData = "0xc1701b67000000000000000000000000b31f66aa3c1e785363f0875a1b74e27b85fd66c7";

try {
  const r = await client.request({
    method: "eth_call" as any,
    params: [{ to: ORACLE_MAIN, data: mainData }, blockHex] as any,
  });
  console.log("Oracle c1701b67 raw:", r);
  
  // Decode the result
  const [priceI, priceII] = decodeAbiParameters([{type:"uint256"},{type:"uint256"}], r as `0x${string}`);
  console.log("Oracle prices:", priceI, priceII);
} catch (e: any) {
  console.log("Error:", e.message);
}

// Now try the oracle trace with debug_traceCall WITHOUT overrides
// vs eth_call with state override of the router
// The difference: state_override sets USDC balance, which might somehow affect the oracle

// Actually let's check: does the oracle check USDC.balanceOf(woopp)?
// From oracle trace: STATICCALL oracle→USDC 0x70a08231 (balanceOf(woopp))
// The oracle checks WooPP's USDC balance!

// In simulation with state override:
// USDC balance slot for DUMMY_SENDER is set
// But WooPP's USDC balance is NOT affected

// So when oracle does USDC.balanceOf(woopp), it gets the real value
// That should be fine... unless the state override of USDC somehow messes things up

// Let me check USDC at the given block
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";

const wooppUSDC = await client.request({
  method: "eth_call" as any,
  params: [{ to: USDC, data: "0x70a08231000000000000000000000000aba7ed514217d51630053d73d358ac2502d3f9bb" }, blockHex] as any,
});
console.log("WooPP USDC balance:", BigInt(wooppUSDC as string));

// Also check if the oracle's result changes with state overrides
const stateOverride = {
  [USDC]: {
    stateDiff: {
      "0x960b1051749987b45b5679007fff577a1c2f763ec21c15a6c5eb193075003785": "0x00000000000000000000000000000000000000000000000000000001270b0180",
    }
  }
};

try {
  const r2 = await client.request({
    method: "eth_call" as any,
    params: [{ to: ORACLE_MAIN, data: mainData }, blockHex, stateOverride] as any,
  });
  const [p1, p2] = decodeAbiParameters([{type:"uint256"},{type:"uint256"}], r2 as `0x${string}`);
  console.log("Oracle with USDC override:", p1, p2);
} catch (e: any) {
  console.log("Oracle with override error:", e.message?.slice(0, 100));
}
