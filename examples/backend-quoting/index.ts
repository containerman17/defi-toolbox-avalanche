// Backend quoting example
//
// Demonstrates pathfinder-based quoting with full EVM verification:
//   1. Sell 1 AVAX for USDC — find the best multi-hop route, quote it
//   2. Buy AVAX back with that USDC — roundtrip to show return rate
//
// Usage:
//   node examples/backend-quoting/index.ts
//
// Requires: state-server running on ws://localhost:7449

import { loadPools } from "../../pool-collector/pools.ts";
import { ERC4626_VAULTS, generateBufferedEdges } from "../../pool-collector/index.ts";
import { buildGraph, findBestRoute } from "../../pathfinder/index.ts";
import { createQuoter, buildStateOverrides } from "../../evm-quoter/sdk.ts";
import { quoteRoute } from "../../router/index.ts";
import { wsPool, closePool } from "../../rpc/ws-pool.ts";
import { createPublicClient } from "viem";
import { avalanche } from "viem/chains";

const STATE_SERVER_URL = process.env.STATE_SERVER_URL || "ws://localhost:7449";
const POOLS_PATH = process.env.POOLS_PATH || "pool-collector/data/pools.txt";

const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const USDC  = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";

const ONE_AVAX = 10n ** 18n;

function formatAvax(amount: bigint): string {
  return (Number(amount) / 1e18).toFixed(6);
}

function formatUsdc(amount: bigint): string {
  return (Number(amount) / 1e6).toFixed(6);
}

function formatRoute(route: { pool: { address: string; providerName: string } }[]): string {
  return route.map(s => `${s.pool.providerName}(${s.pool.address.slice(0, 10)})`).join(" → ");
}

async function main() {
  // 1. Load pools + synthetic edges
  const { pools } = loadPools(POOLS_PATH);
  const poolList = [...pools.values()].slice(0, 1000);
  const buffered = generateBufferedEdges(poolList);
  const allPools = [...poolList, ...ERC4626_VAULTS, ...buffered];

  console.log(`Loaded ${poolList.length} pools + ${ERC4626_VAULTS.length} ERC4626 + ${buffered.length} buffered\n`);

  // 2. Build pathfinder graph
  const graph = buildGraph(allPools);

  // 3. State overrides for local EVM quoting
  const { overrides } = buildStateOverrides(allPools, {
    tokenAmounts: new Map([
      [WAVAX, 1000n * ONE_AVAX],
      [USDC, 1_000_000n * 10n ** 6n],
    ]),
  });

  // 4. Create local EVM quoter (fast, for pathfinding)
  const quoter = await createQuoter("native", { stateServerUrl: STATE_SERVER_URL });

  // 5. Create viem client (for full on-chain verification via eth_call)
  const transport = wsPool(STATE_SERVER_URL);
  const client = createPublicClient({ chain: avalanche, transport });

  // ── Sell 1 AVAX → USDC ────────────────────────────────────────────

  console.log(`Selling 1 AVAX for USDC...`);
  const t0 = Date.now();
  const sell = await findBestRoute(quoter, graph, WAVAX, USDC, ONE_AVAX, overrides);

  if (!sell) {
    console.error("No route found for AVAX → USDC");
    quoter.close();
    closePool(STATE_SERVER_URL);
    process.exit(1);
  }

  const sellMs = Date.now() - t0;
  console.log(`  Route:     ${formatRoute(sell.route)}`);
  console.log(`  Hops:      ${sell.route.length}`);
  console.log(`  Amount in: ${formatAvax(ONE_AVAX)} AVAX`);
  console.log(`  Quote out: ${formatUsdc(sell.amountOut)} USDC`);
  console.log(`  EVM calls: ${sell.stats.evmCalls} (${sell.stats.cacheMisses} cache misses)`);
  console.log(`  Time:      ${sellMs}ms`);

  // Verify with full eth_call
  let verifiedSellOut: bigint | null = null;
  try {
    verifiedSellOut = await quoteRoute(client, sell.route, ONE_AVAX);
    console.log(`  Verified:  ${formatUsdc(verifiedSellOut)} USDC`);
  } catch (err: any) {
    console.log(`  Verify:    failed (${err.message?.slice(0, 60)})`);
  }

  // ── Buy AVAX back with that USDC ──────────────────────────────────

  const usdcAmount = sell.amountOut;
  console.log(`\nBuying AVAX back with ${formatUsdc(usdcAmount)} USDC...`);
  const t1 = Date.now();
  const buy = await findBestRoute(quoter, graph, USDC, WAVAX, usdcAmount, overrides);

  if (!buy) {
    console.error("No route found for USDC → AVAX");
    quoter.close();
    closePool(STATE_SERVER_URL);
    process.exit(1);
  }

  const buyMs = Date.now() - t1;
  console.log(`  Route:     ${formatRoute(buy.route)}`);
  console.log(`  Hops:      ${buy.route.length}`);
  console.log(`  Amount in: ${formatUsdc(usdcAmount)} USDC`);
  console.log(`  Quote out: ${formatAvax(buy.amountOut)} AVAX`);
  console.log(`  EVM calls: ${buy.stats.evmCalls} (${buy.stats.cacheMisses} cache misses)`);
  console.log(`  Time:      ${buyMs}ms`);

  // Verify with full eth_call
  let verifiedBuyOut: bigint | null = null;
  try {
    verifiedBuyOut = await quoteRoute(client, buy.route, usdcAmount);
    console.log(`  Verified:  ${formatAvax(verifiedBuyOut)} AVAX`);
  } catch (err: any) {
    console.log(`  Verify:    failed (${err.message?.slice(0, 60)})`);
  }

  // ── Roundtrip summary ─────────────────────────────────────────────

  const returnRate = Number(buy.amountOut * 10000n / ONE_AVAX) / 100;

  console.log(`\n${"─".repeat(50)}`);
  console.log(`Roundtrip: 1 AVAX → ${formatUsdc(sell.amountOut)} USDC → ${formatAvax(buy.amountOut)} AVAX`);
  console.log(`Return:    ${returnRate.toFixed(2)}%`);
  if (verifiedBuyOut) {
    const verifiedRate = Number(verifiedBuyOut * 10000n / ONE_AVAX) / 100;
    console.log(`Verified:  ${verifiedRate.toFixed(2)}%`);
  }
  console.log(`Total:     ${sellMs + buyMs}ms`);

  // Cleanup
  quoter.close();
  closePool(STATE_SERVER_URL);
  process.exit(0);
}

main().catch(err => {
  console.error(err);
  process.exit(1);
});
