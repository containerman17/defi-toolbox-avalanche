// Check if oracle returns correct values with vs without state overrides
import { createPublicClient, http, decodeAbiParameters, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { getBalanceOverride, getAllowanceOverride, ROUTER_ADDRESS } from "hayabusa-router";
import { readFileSync } from "node:fs";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const BYTECODE = "0x" + readFileSync("/home/claude/defi-toolbox-avalanche/router/contracts/bytecode.hex", "utf-8").trim();

const ORACLE = "0xd92e3c8f1c5e835e4e76173c4e83bf517f61b737";
const FEED = "0x5ecf662abb67a92f6a8c8cc050a0b1e41d7d9c3a"; // address from trace
const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const amountIn = 4950000000n;
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;
const DUMMY_SENDER = "0x000000000000000000000000000000000000dEaD";

// Oracle has selector 0xc1701b67(address baseToken)
// Let's call it WITHOUT state overrides
const oracleCallData = "0xc1701b67" + WAVAX.slice(2).padStart(64, "0");

// Without state overrides
const oracleResultBare = await client.request({
  method: "eth_call" as any,
  params: [{ to: ORACLE, data: oracleCallData }, blockHex] as any,
});
console.log("Oracle result WITHOUT overrides:", oracleResultBare);
if (oracleResultBare && oracleResultBare !== "0x") {
  try {
    const [priceNow, feasible] = decodeAbiParameters([{type:"uint256"},{type:"bool"}], oracleResultBare as Hex);
    console.log("  price:", priceNow, "feasible:", feasible);
  } catch(e) {}
}

// Now check what happens when oracle is called WITH state overrides
const balOverride = getBalanceOverride(USDC, amountIn, DUMMY_SENDER);
const allowOverride = getAllowanceOverride(USDC, DUMMY_SENDER, ROUTER_ADDRESS);
const merged: Record<string, Record<string, Hex>> = {};
for (const ovr of [balOverride, allowOverride]) {
  for (const [addr, val] of Object.entries(ovr)) {
    if (!merged[addr]) merged[addr] = {};
    Object.assign(merged[addr], val.stateDiff);
  }
}
const stateOverride: Record<string, any> = {};
for (const [address, slots] of Object.entries(merged)) {
  stateOverride[address] = { stateDiff: slots };
}
stateOverride[ROUTER_ADDRESS] = { code: BYTECODE as Hex };

const oracleResultWith = await client.request({
  method: "eth_call" as any,
  params: [{ to: ORACLE, data: oracleCallData }, blockHex, stateOverride] as any,
});
console.log("\nOracle result WITH overrides:", oracleResultWith);

// Also call oracle from WooPP context to check feasibility
// Let's check if oracle checks feasibility based on WAVAX/USDC balances
// The oracle in the trace makes calls to 0x5ecf662abb
// And also checks WAVAX balance and USDC balance
// Those balance calls returned 0 - maybe that's why feasible=false?

// Check WAVAX balance of oracle
const wavaxBalOracle = await client.request({
  method: "eth_call" as any,
  params: [{ to: WAVAX, data: "0x70a08231" + ORACLE.slice(2).padStart(64, "0") }, blockHex] as any,
});
console.log("\nOracle WAVAX balance:", BigInt(wavaxBalOracle as string));

const wavaxBalWoopp = await client.request({
  method: "eth_call" as any,
  params: [{ to: WAVAX, data: "0x70a08231" + WOOPP.slice(2).padStart(64, "0") }, blockHex] as any,
});
console.log("WooPP WAVAX balance:", BigInt(wavaxBalWoopp as string));

// Check if the oracle's `state` or feasibility depends on chainlink/pyth feeds
// Let's decode the oracle call data more carefully from the trace
// The trace shows: STATICCALL 0xd92e3c8f1c→0x5ecf662abb 0xb5e4b813e3faa9d0
// Selector 0xb5e4b813 = ?
// STATICCALL 0xd92e3c8f1c→0x5ecf662abb 0xb5e4b8133f640011 = also 0xb5e4b813
// STATICCALL 0xd92e3c8f1c→0x5ecf662abb 0x893ddf87e3faa9d0 = 0x893ddf87 = ?
console.log("\n=== Oracle prestate in REAL tx vs simulation ===");

// Real tx prestate for oracle
const realPrestate = await client.request({
  method: "debug_traceTransaction" as any,
  params: ["0x1f84c56947f5d4e8d12096ea6c4622f368405b87f96f0924d8775453cea4bbff", {
    tracer: "prestateTracer",
    tracerConfig: { diffMode: false }
  }] as any,
}) as any;

const oracleKey = Object.keys(realPrestate).find(k => k.toLowerCase() === ORACLE.toLowerCase());
if (oracleKey) {
  console.log("Oracle storage in real tx:", JSON.stringify(realPrestate[oracleKey].storage, null, 2));
}

// Simulation prestate
const simPrestate = await client.request({
  method: "debug_traceCall" as any,
  params: [
    { from: DUMMY_SENDER, to: ORACLE, data: oracleCallData, gas: "0x989680" },
    blockHex,
    { tracer: "prestateTracer", tracerConfig: { diffMode: false }, stateOverrides: stateOverride },
  ] as any,
}) as any;

const simOracleKey = Object.keys(simPrestate).find(k => k.toLowerCase() === ORACLE.toLowerCase());
if (simOracleKey) {
  console.log("Oracle storage in simulation:", JSON.stringify(simPrestate[simOracleKey].storage, null, 2));
}

// Check if they differ
const feedKey5ecf = Object.keys(realPrestate).find(k => k.toLowerCase().includes("5ecf"));
if (feedKey5ecf) {
  console.log("\nFeed (5ecf...) storage:", JSON.stringify(realPrestate[feedKey5ecf].storage, null, 2));
}
