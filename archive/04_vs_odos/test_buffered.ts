import * as fs from "node:fs";
import path from "node:path";
import { createPublicClient, http, type Hex, encodeFunctionData, parseAbi, decodeAbiParameters, encodeAbiParameters, getAddress } from "viem";
import { avalanche } from "viem/chains";
import { loadDotEnv } from "../../utils/env.ts";
import { getBalanceOverride, getAllowanceOverride } from "../../router/overrides.ts";

loadDotEnv();

const rpcUrl = process.env.RPC_URL || "https://api.avax.network/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const block = 80341389n;
const QUOTER = getAddress("0x00000000000000000000000000000000deadbeef");
const DUMMY = "0x000000000000000000000000000000000000dEaD" as Hex;

const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7" as Hex;
const sAVAX = "0x2b2c81e08f1af8835a78bb2a90ae924ace0ea4be" as Hex;
const waAvaWAVAX = "0xd7da0de6ef4f51d6206bf2a35fcd2030f54c3f7b" as Hex;
const waAvaSAVAX = "0x7d0394f8898fba73836bf12bd606228887705895" as Hex;
const balancerPool = "0x99a9a471dbe0dcc6855b4cd4bbabeccb1280f5e8" as Hex;

const amountIn = 1000000000000000000n; // 1 AVAX

const quoterBytecode = ("0x" + fs.readFileSync(path.join(import.meta.dirname, "../../router/contracts/bytecode.hex"), "utf-8").trim()) as Hex;

// Encode extra_data: abi.encode(wrappedIn, pool, wrappedOut)
const extraData = encodeAbiParameters(
  [{ type: "address" }, { type: "address" }, { type: "address" }],
  [waAvaWAVAX, balancerPool, waAvaSAVAX]
);

// Test 1: quoteMulti (revert trick) with the buffered pool type (11)
const quoteMultiAbi = parseAbi([
  "function quoteMulti(address pool, uint8 poolType, address tokenIn, address tokenOut, uint256[] amounts, bytes extraData) external returns (uint256[])",
]);

const calldata = encodeFunctionData({
  abi: quoteMultiAbi,
  functionName: "quoteMulti",
  args: [
    balancerPool, // pool address (not really used, just for routing)
    11,           // BALANCER_V3_BUFFERED
    WAVAX,
    sAVAX,
    [amountIn],
    extraData as Hex,
  ],
});

// State overrides: quoter bytecode + WAVAX balance on quoter
const overrides: Record<string, any> = {
  [QUOTER]: { code: quoterBytecode },
};
const balOvr = getBalanceOverride(WAVAX, amountIn * 10n, QUOTER);
const allowOvr = getAllowanceOverride(WAVAX, QUOTER, QUOTER);
for (const ovr of [balOvr, allowOvr]) {
  for (const [addr, val] of Object.entries(ovr)) {
    if (!overrides[addr]) overrides[addr] = { stateDiff: {} };
    if (!overrides[addr].stateDiff) overrides[addr].stateDiff = {};
    Object.assign(overrides[addr].stateDiff, val.stateDiff);
  }
}

function buildStateOverride() {
  return Object.entries(overrides).map(([addr, val]: [string, any]) => {
    const entry: any = { address: addr as Hex };
    if (val.code) entry.code = val.code;
    if (val.stateDiff) {
      entry.stateDiff = Object.entries(val.stateDiff).map(([slot, value]) => ({
        slot: slot as Hex,
        value: value as Hex,
      }));
    }
    return entry;
  });
}

console.log("=== Test: Balancer V3 Buffered (type=11) WAVAX → sAVAX ===");
console.log("extraData:", extraData);

try {
  const result = await client.call({
    account: DUMMY,
    to: QUOTER,
    data: calldata,
    stateOverride: buildStateOverride(),
    blockNumber: block,
  });

  if (result.data) {
    const [amounts] = decodeAbiParameters([{ type: "uint256[]" }], result.data);
    console.log("amount_out:", amounts[0].toString());
    console.log("amount_out (decimal):", Number(amounts[0]) / 1e18, "sAVAX");

    // Compare with Odos
    const odosAmount = 798126872444040064n;
    const diff = Number(amounts[0]) - Number(odosAmount);
    const pct = (diff / Number(odosAmount)) * 100;
    console.log(`\nOdos amount:    ${odosAmount.toString()}`);
    console.log(`Our amount:     ${amounts[0].toString()}`);
    console.log(`Diff:           ${diff} (${pct >= 0 ? "+" : ""}${pct.toFixed(6)}%)`);
  } else {
    console.log("No output data");
  }
} catch (e: any) {
  console.error("Error:", e.message?.slice(0, 500));
}

// Test 2: quoteRoute (multi-hop execution) with the buffered type as single hop
const quoteRouteAbi = parseAbi([
  "function quoteRoute(address[] pools, uint8[] poolTypes, address[] tokens, uint256 amountIn, bytes[] extraDatas) external returns (uint256)",
]);

const routeCalldata = encodeFunctionData({
  abi: quoteRouteAbi,
  functionName: "quoteRoute",
  args: [
    [balancerPool],          // 1 pool
    [11],                     // BALANCER_V3_BUFFERED
    [WAVAX, sAVAX],          // 2 tokens (in, out)
    amountIn,
    [extraData as Hex],
  ],
});

console.log("\n=== Test: quoteRoute with Balancer V3 Buffered ===");
try {
  const result = await client.call({
    account: DUMMY,
    to: QUOTER,
    data: routeCalldata,
    stateOverride: buildStateOverride(),
    blockNumber: block,
  });

  if (result.data) {
    const [amountOut] = decodeAbiParameters([{ type: "uint256" }], result.data);
    console.log("amount_out:", amountOut.toString());
    console.log("amount_out (decimal):", Number(amountOut) / 1e18, "sAVAX");
  } else {
    console.log("No output data");
  }
} catch (e: any) {
  console.error("Error:", e.message?.slice(0, 500));
}

process.exit(0);
