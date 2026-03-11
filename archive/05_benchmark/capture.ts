// Captures Odos ground truth for benchmark blocks.
// Usage: node capture.ts          # capture at latest block
//        node capture.ts 80341389 # capture at specific block
//
// Output: captures/<block>.json

import * as fs from "node:fs";
import path from "node:path";
import { createPublicClient, http, formatUnits, type Hex, decodeAbiParameters } from "viem";
import { avalanche } from "viem/chains";
import { loadDotEnv } from "../../utils/env.ts";
import { getBalanceOverride, getAllowanceOverride } from "../../router/overrides.ts";

loadDotEnv();

const DUMMY_SENDER = "0x000000000000000000000000000000000000dEaD";
const WAVAX  = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const USDC   = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const USDt   = "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7";
const sAVAX  = "0x2b2c81e08f1af8835a78bb2a90ae924ace0ea4be";
const JOE    = "0x6e84a6216ea6dacc71ee8e6b0a5b7322eebc0fdd";
const AAVE   = "0x63a72806098bd3d9520cc43356dd78afe5d386d9";
const ggAVAX = "0xa25eaf2906fa1a3a13edac9b9657108af7b703e3";
const ONE_AVAX = 1_000_000_000_000_000_000n;
const ODOS_API = "https://api.odos.xyz";

const rpcUrl = process.env.RPC_URL || "http://localhost:9650/ext/bc/C/rpc";
const httpClient = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

interface QuotePair {
  name: string;
  tokenIn: string;
  tokenOut: string;
  amount: bigint;
  decimalsIn: number;
  decimalsOut: number;
}

// Helper to build pairs concisely
function p(name: string, tokenIn: string, tokenOut: string, amount: bigint, decimalsIn: number, decimalsOut: number): QuotePair {
  return { name, tokenIn, tokenOut, amount, decimalsIn, decimalsOut };
}

const e6 = (n: number) => BigInt(n) * 1_000_000n;
const e18 = (n: number) => BigInt(n) * ONE_AVAX;

const PAIRS: QuotePair[] = [
  // ── AVAX → Stablecoins (various sizes) ──
  p("1 AVAX → USDC",         WAVAX, USDC,  e18(1),     18, 6),
  p("5 AVAX → USDC",         WAVAX, USDC,  e18(5),     18, 6),
  p("10 AVAX → USDC",        WAVAX, USDC,  e18(10),    18, 6),
  p("50 AVAX → USDC",        WAVAX, USDC,  e18(50),    18, 6),
  p("100 AVAX → USDC",       WAVAX, USDC,  e18(100),   18, 6),
  p("200 AVAX → USDC",       WAVAX, USDC,  e18(200),   18, 6),
  p("500 AVAX → USDC",       WAVAX, USDC,  e18(500),   18, 6),
  p("1000 AVAX → USDC",      WAVAX, USDC,  e18(1000),  18, 6),
  p("2000 AVAX → USDC",      WAVAX, USDC,  e18(2000),  18, 6),
  p("1 AVAX → USDt",         WAVAX, USDt,  e18(1),     18, 6),
  p("10 AVAX → USDt",        WAVAX, USDt,  e18(10),    18, 6),
  p("100 AVAX → USDt",       WAVAX, USDt,  e18(100),   18, 6),
  p("200 AVAX → USDt",       WAVAX, USDt,  e18(200),   18, 6),

  // ── Stablecoins → AVAX ──
  p("10 USDC → WAVAX",       USDC,  WAVAX, e6(10),     6, 18),
  p("100 USDC → WAVAX",      USDC,  WAVAX, e6(100),    6, 18),
  p("1000 USDC → WAVAX",     USDC,  WAVAX, e6(1000),   6, 18),
  p("5000 USDC → WAVAX",     USDC,  WAVAX, e6(5000),   6, 18),
  p("10000 USDC → WAVAX",    USDC,  WAVAX, e6(10000),  6, 18),
  p("10 USDt → WAVAX",       USDt,  WAVAX, e6(10),     6, 18),
  p("100 USDt → WAVAX",      USDt,  WAVAX, e6(100),    6, 18),
  p("500 USDt → WAVAX",      USDt,  WAVAX, e6(500),    6, 18),
  p("1000 USDt → WAVAX",     USDt,  WAVAX, e6(1000),   6, 18),
  p("5000 USDt → WAVAX",     USDt,  WAVAX, e6(5000),   6, 18),

  // ── Stablecoin ↔ Stablecoin ──
  p("50 USDC → USDt",        USDC,  USDt,  e6(50),     6, 6),
  p("500 USDC → USDt",       USDC,  USDt,  e6(500),    6, 6),
  p("5000 USDC → USDt",      USDC,  USDt,  e6(5000),   6, 6),
  p("10000 USDC → USDt",     USDC,  USDt,  e6(10000),  6, 6),
  p("50000 USDC → USDt",     USDC,  USDt,  e6(50000),  6, 6),
  p("50 USDt → USDC",        USDt,  USDC,  e6(50),     6, 6),
  p("500 USDt → USDC",       USDt,  USDC,  e6(500),    6, 6),
  p("5000 USDt → USDC",      USDt,  USDC,  e6(5000),   6, 6),
  p("10000 USDt → USDC",     USDt,  USDC,  e6(10000),  6, 6),
  p("50000 USDt → USDC",     USDt,  USDC,  e6(50000),  6, 6),

  // ── AVAX ↔ LSTs ──
  p("1 AVAX → sAVAX",        WAVAX, sAVAX, e18(1),     18, 18),
  p("10 AVAX → sAVAX",       WAVAX, sAVAX, e18(10),    18, 18),
  p("100 AVAX → sAVAX",      WAVAX, sAVAX, e18(100),   18, 18),
  p("10 sAVAX → WAVAX",      sAVAX, WAVAX, e18(10),    18, 18),
  p("100 sAVAX → WAVAX",     sAVAX, WAVAX, e18(100),   18, 18),
  p("1 AVAX → ggAVAX",       WAVAX, ggAVAX, e18(1),    18, 18),
  p("10 AVAX → ggAVAX",      WAVAX, ggAVAX, e18(10),   18, 18),
  p("10 ggAVAX → WAVAX",     ggAVAX, WAVAX, e18(10),   18, 18),

  // ── Cross via LSTs ──
  p("10 sAVAX → USDC",       sAVAX, USDC,  e18(10),    18, 6),
  p("10 ggAVAX → USDC",      ggAVAX, USDC, e18(10),    18, 6),
  p("10 sAVAX → USDt",       sAVAX, USDt,  e18(10),    18, 6),
  p("10 ggAVAX → USDt",      ggAVAX, USDt, e18(10),    18, 6),
  p("100 USDC → sAVAX",      USDC,  sAVAX, e6(100),    6, 18),
  p("100 USDC → ggAVAX",     USDC,  ggAVAX, e6(100),   6, 18),

  // ── JOE token ──
  p("100 AVAX → JOE",        WAVAX, JOE,   e18(100),   18, 18),
  p("10000 JOE → WAVAX",     JOE,   WAVAX, e18(10000), 18, 18),
  p("10000 JOE → USDC",      JOE,   USDC,  e18(10000), 18, 6),
  p("1000 JOE → USDC",       JOE,   USDC,  e18(1000),  18, 6),
  p("100 USDC → JOE",        USDC,  JOE,   e6(100),    6, 18),

  // ── AAVE ──
  p("10 AVAX → AAVE",        WAVAX, AAVE,  e18(10),    18, 18),
  p("10 AAVE → WAVAX",       AAVE,  WAVAX, e18(10),    18, 18),
  p("10 AAVE → USDC",        AAVE,  USDC,  e18(10),    18, 6),
  p("100 USDC → AAVE",       USDC,  AAVE,  e6(100),    6, 18),

  // ── Extra size coverage ──
  p("20 AVAX → USDC",        WAVAX, USDC,  e18(20),    18, 6),
  p("20 AVAX → USDt",        WAVAX, USDt,  e18(20),    18, 6),
  p("1000 USDC → sAVAX",     USDC,  sAVAX, e6(1000),   6, 18),
  p("1000 USDC → ggAVAX",    USDC,  ggAVAX, e6(1000),  6, 18),
];

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
  if (!qResp.ok) return null;
  const q = await qResp.json();
  if (!q.pathId) return null;

  const aResp = await fetch(`${ODOS_API}/sor/assemble`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ pathId: q.pathId, userAddr: DUMMY_SENDER }),
  });
  if (!aResp.ok) return null;
  const a = await aResp.json();
  if (!a.transaction) return null;

  return { txTo: a.transaction.to, txData: a.transaction.data, pathViz: q.pathViz, gasEstimate: q.gasEstimate, outAmounts: q.outAmounts };
}

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

async function simulateOdosTx(txTo: string, txData: string, tokenIn: string, amountIn: bigint, block: bigint): Promise<bigint | null> {
  try {
    const result = await httpClient.call({
      account: DUMMY_SENDER as Hex,
      to: txTo as Hex,
      data: txData as Hex,
      stateOverride: buildStateOverrides(tokenIn, amountIn, txTo),
      blockNumber: block,
    });
    if (!result.data || result.data === "0x") return null;
    const [amountOut] = decodeAbiParameters([{ type: "uint256" }], result.data);
    return amountOut;
  } catch { return null; }
}

// ── Main ──
const requestedBlock = process.argv[2] ? BigInt(process.argv[2]) : null;
const latestBlock = await httpClient.getBlockNumber();
const targetBlock = requestedBlock ?? (latestBlock - 1n);

console.log(`Capturing Odos quotes at block ${targetBlock}...\n`);

const odosResults = await Promise.all(
  PAIRS.map(p => odosQuoteAndAssemble(p.tokenIn, p.tokenOut, p.amount).catch(() => null))
);

const simResults = await Promise.all(
  PAIRS.map((p, i) => {
    const odos = odosResults[i];
    if (!odos?.txTo || !odos?.txData) return Promise.resolve(null);
    return simulateOdosTx(odos.txTo, odos.txData, p.tokenIn, p.amount, targetBlock);
  })
);

const pairs: any[] = [];
for (let i = 0; i < PAIRS.length; i++) {
  const p = PAIRS[i];
  const odos = odosResults[i];
  const sim = simResults[i];
  const outStr = sim !== null ? formatUnits(sim, p.decimalsOut) : "FAIL";
  console.log(`  ${p.name}: ${outStr}`);

  pairs.push({
    name: p.name,
    token_in: p.tokenIn,
    token_out: p.tokenOut,
    amount_in: p.amount.toString(),
    decimals_in: p.decimalsIn,
    decimals_out: p.decimalsOut,
    odos_tx_to: odos?.txTo ?? null,
    odos_tx_data: odos?.txData ?? null,
    odos_simulated_out: sim?.toString() ?? null,
    odos_path_viz: odos?.pathViz ?? null,
  });
}

const outDir = path.join(import.meta.dirname, "captures");
fs.mkdirSync(outDir, { recursive: true });
const outFile = path.join(outDir, `${targetBlock}.json`);
fs.writeFileSync(outFile, JSON.stringify({ block: Number(targetBlock), timestamp: new Date().toISOString(), pairs }, null, 2));
console.log(`\nSaved: ${outFile}`);
