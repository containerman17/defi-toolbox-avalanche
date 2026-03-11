// Captures Odos quotes for all pairs, simulates them on-chain, saves to JSON.
// Usage: node capture_odos.ts
// Output: captures/<block>.json

import * as fs from "node:fs";
import path from "node:path";
import { createPublicClient, http, formatUnits, type Hex, decodeAbiParameters } from "viem";
import { avalanche } from "viem/chains";
import { loadDotEnv } from "../../utils/env.ts";
import { getBalanceOverride, getAllowanceOverride } from "../../router/overrides.ts";

loadDotEnv();

const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const USDC  = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const USDt  = "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7";
const sAVAX = "0x2b2c81e08f1af8835a78bb2a90ae924ace0ea4be";
const ONE_AVAX = 1_000_000_000_000_000_000n;

interface QuotePair {
  name: string;
  tokenIn: string;
  tokenOut: string;
  amount: bigint;
  decimalsIn: number;
  decimalsOut: number;
}

const PAIRS: QuotePair[] = [
  { name: "10 USDC → WAVAX",   tokenIn: USDC,  tokenOut: WAVAX, amount: 10_000_000n,      decimalsIn: 6,  decimalsOut: 18 },
  { name: "100 USDC → WAVAX",  tokenIn: USDC,  tokenOut: WAVAX, amount: 100_000_000n,     decimalsIn: 6,  decimalsOut: 18 },
  { name: "1 AVAX → USDC",     tokenIn: WAVAX, tokenOut: USDC,  amount: ONE_AVAX,         decimalsIn: 18, decimalsOut: 6 },
  { name: "10 AVAX → USDC",    tokenIn: WAVAX, tokenOut: USDC,  amount: 10n * ONE_AVAX,   decimalsIn: 18, decimalsOut: 6 },
  { name: "10 USDC → USDt",    tokenIn: USDC,  tokenOut: USDt,  amount: 10_000_000n,      decimalsIn: 6,  decimalsOut: 6 },
  { name: "100 USDC → USDt",   tokenIn: USDC,  tokenOut: USDt,  amount: 100_000_000n,     decimalsIn: 6,  decimalsOut: 6 },
  { name: "1 AVAX → sAVAX",    tokenIn: WAVAX, tokenOut: sAVAX, amount: ONE_AVAX,         decimalsIn: 18, decimalsOut: 18 },
  { name: "10 AVAX → sAVAX",   tokenIn: WAVAX, tokenOut: sAVAX, amount: 10n * ONE_AVAX,   decimalsIn: 18, decimalsOut: 18 },
  { name: "5 AVAX → USDt",     tokenIn: WAVAX, tokenOut: USDt,  amount: 5n * ONE_AVAX,    decimalsIn: 18, decimalsOut: 6 },
  { name: "50 USDt → WAVAX",   tokenIn: USDt,  tokenOut: WAVAX, amount: 50_000_000n,      decimalsIn: 6,  decimalsOut: 18 },
];

const DUMMY_SENDER = "0x000000000000000000000000000000000000dEaD";
const ODOS_API = "https://api.odos.xyz";

const rpcUrl = process.env.RPC_URL || "https://api.avax.network/ext/bc/C/rpc";
const httpClient = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

// ── Odos API ──
async function odosQuoteAndAssemble(tokenIn: string, tokenOut: string, amount: bigint) {
  const qResp = await fetch(`${ODOS_API}/sor/quote/v3`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      chainId: 43114,
      inputTokens: [{ tokenAddress: tokenIn, amount: amount.toString() }],
      outputTokens: [{ tokenAddress: tokenOut, proportion: 1 }],
      slippageLimitPercent: 50,
      userAddr: DUMMY_SENDER,
      referralCode: 0,
      pathViz: true,
    }),
  });
  if (!qResp.ok) throw new Error(`Odos quote failed: ${qResp.status}`);
  const q = await qResp.json();
  if (!q.pathId) throw new Error(`Odos quote: no pathId`);

  const aResp = await fetch(`${ODOS_API}/sor/assemble`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ pathId: q.pathId, userAddr: DUMMY_SENDER }),
  });
  if (!aResp.ok) throw new Error(`Odos assemble failed: ${aResp.status}`);
  const a = await aResp.json();
  if (!a.transaction) throw new Error(`Odos assemble: no transaction`);

  return {
    pathViz: q.pathViz,
    gasEstimate: q.gasEstimate,
    outAmounts: q.outAmounts,
    txTo: a.transaction.to as string,
    txData: a.transaction.data as string,
  };
}

// ── Simulate tx via eth_call ──
function buildStateOverrides(tokenIn: string, amountIn: bigint, spender: string) {
  const balOvr = getBalanceOverride(tokenIn, amountIn, DUMMY_SENDER);
  const allowOvr = getAllowanceOverride(tokenIn, DUMMY_SENDER, spender);
  const merged: Record<string, Record<string, Hex>> = {};
  for (const ovr of [balOvr, allowOvr]) {
    for (const [addr, val] of Object.entries(ovr)) {
      if (!merged[addr]) merged[addr] = {};
      Object.assign(merged[addr], val.stateDiff);
    }
  }
  return Object.entries(merged).map(([address, slots]) => ({
    address: address as Hex,
    stateDiff: Object.entries(slots).map(([slot, value]) => ({ slot: slot as Hex, value: value as Hex })),
  }));
}

async function simulateTx(to: string, data: string, tokenIn: string, amountIn: bigint, blockNumber: bigint): Promise<bigint> {
  const result = await httpClient.call({
    account: DUMMY_SENDER as Hex,
    to: to as Hex,
    data: data as Hex,
    stateOverride: buildStateOverrides(tokenIn, amountIn, to),
    blockNumber,
  });
  if (!result.data || result.data === "0x") throw new Error("simulation returned empty");
  const [amountOut] = decodeAbiParameters([{ type: "uint256" }], result.data);
  return amountOut;
}

// ── Main ──
console.log("Capturing Odos quotes...\n");

// Get block number in parallel with Odos quotes
const [block, ...odosResults] = await Promise.all([
  httpClient.getBlockNumber(),
  ...PAIRS.map(p => odosQuoteAndAssemble(p.tokenIn, p.tokenOut, p.amount).catch(e => {
    console.error(`  ${p.name}: ${e.message}`);
    return null;
  })),
]);

const verifyBlock = block - 1n; // use latest-1 for safety
console.log(`Block: ${verifyBlock}\n`);

// Simulate all Odos txs at this block
const pairs: any[] = [];
for (let i = 0; i < PAIRS.length; i++) {
  const pair = PAIRS[i];
  const odos = odosResults[i];
  if (!odos) {
    console.log(`  ${pair.name}: FAILED (no Odos quote)`);
    pairs.push({
      name: pair.name,
      token_in: pair.tokenIn,
      token_out: pair.tokenOut,
      amount_in: pair.amount.toString(),
      decimals_in: pair.decimalsIn,
      decimals_out: pair.decimalsOut,
      odos: null,
    });
    continue;
  }

  let simulated: bigint | null = null;
  try {
    simulated = await simulateTx(odos.txTo, odos.txData, pair.tokenIn, pair.amount, verifyBlock);
  } catch (e: any) {
    console.error(`  ${pair.name}: simulation failed: ${e.message}`);
  }

  const outStr = simulated !== null ? formatUnits(simulated, pair.decimalsOut) : "FAIL";
  console.log(`  ${pair.name}: ${outStr}`);

  pairs.push({
    name: pair.name,
    token_in: pair.tokenIn,
    token_out: pair.tokenOut,
    amount_in: pair.amount.toString(),
    decimals_in: pair.decimalsIn,
    decimals_out: pair.decimalsOut,
    odos: {
      tx_to: odos.txTo,
      tx_data: odos.txData,
      path_viz: odos.pathViz,
      gas_estimate: odos.gasEstimate,
      api_out_amounts: odos.outAmounts,
      simulated_out: simulated?.toString() ?? null,
    },
  });
}

// Save
const outDir = path.join(import.meta.dirname, "captures");
fs.mkdirSync(outDir, { recursive: true });
const outFile = path.join(outDir, `${verifyBlock}.json`);
const artifact = {
  block: Number(verifyBlock),
  timestamp: new Date().toISOString(),
  pairs,
};
fs.writeFileSync(outFile, JSON.stringify(artifact, null, 2));
console.log(`\nSaved: ${outFile}`);
