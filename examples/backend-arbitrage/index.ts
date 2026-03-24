// Backend arbitrage example
//
// Scans WAVAX roundtrip opportunities across all tokens in the pool graph.
// For each reachable token, tries 0.01 / 0.05 / 0.1 AVAX roundtrips using
// the BFS pathfinder with local EVM quoting. Profitable routes are verified
// with a full eth_call quote through the Hayabusa router.
//
// Usage:
//   node examples/backend-arbitrage/index.ts              # scan top 1000 pools
//   node examples/backend-arbitrage/index.ts 500          # scan top 500 pools
//
// Requires: state-server running on ws://localhost:7449

import { loadPools } from "../../pool-collector/pools.ts";
import { ERC4626_VAULTS, generateBufferedEdges } from "../../pool-collector/index.ts";
import { buildGraph, findBestRoute } from "../../pathfinder/index.ts";
import { createQuoter, buildStateOverrides } from "../../evm-quoter/sdk.ts";
import { quoteRoute, ROUTER_ADDRESS } from "../../router/index.ts";
import { wsPool, closePool } from "../../rpc/ws-pool.ts";
import { createPublicClient } from "viem";
import { avalanche } from "viem/chains";

const STATE_SERVER_URL = process.env.STATE_SERVER_URL || "ws://localhost:7449";
const POOLS_PATH = process.env.POOLS_PATH || "pool-collector/data/pools.txt";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";

const AMOUNTS = [
  { wavax: 10n ** 16n,      label: "0.01 AVAX" },
  { wavax: 5n * 10n ** 16n, label: "0.05 AVAX" },
  { wavax: 10n ** 17n,      label: "0.1 AVAX"  },
] as const;

async function main() {
  const maxPools = parseInt(process.argv[2] || "1000", 10);

  // 1. Load pools + synthetic edges
  const { pools } = loadPools(POOLS_PATH);
  const poolList = [...pools.values()].slice(0, maxPools);
  const buffered = generateBufferedEdges(poolList);
  const allPools = [...poolList, ...ERC4626_VAULTS, ...buffered];

  console.log(`Loaded ${poolList.length} pools + ${ERC4626_VAULTS.length} ERC4626 + ${buffered.length} buffered edges`);

  // 2. Build graph and collect all reachable tokens from WAVAX
  const graph = buildGraph(allPools);
  const wavaxEdges = graph.edges.get(WAVAX);
  if (!wavaxEdges) {
    console.error("No edges from WAVAX found");
    process.exit(1);
  }

  const targetTokens = [...new Set(wavaxEdges.map(e => e.tokenOut))].filter(t => t !== WAVAX);
  console.log(`Found ${targetTokens.length} tokens directly reachable from WAVAX\n`);

  // 3. Build state overrides
  const { overrides } = buildStateOverrides(allPools, {
    tokenAmounts: new Map([[WAVAX, 1000n * 10n ** 18n]]),
  });

  // 4. Create local EVM quoter for pathfinding
  const quoter = await createQuoter("native", { stateServerUrl: STATE_SERVER_URL });

  // 5. Create viem client for full route verification
  const transport = wsPool(STATE_SERVER_URL);
  const client = createPublicClient({ chain: avalanche, transport });

  // 6. Scan roundtrips
  const opportunities: {
    token: string;
    amount: string;
    profitBps: number;
    forwardHops: number;
    reverseHops: number;
    verified: boolean;
    verifiedProfitBps?: number;
  }[] = [];

  for (const token of targetTokens) {
    for (const amt of AMOUNTS) {
      try {
        // Forward: WAVAX → token
        const forward = await findBestRoute(quoter, graph, WAVAX, token, amt.wavax, overrides);
        if (!forward) continue;

        // Reverse: token → WAVAX
        const reverse = await findBestRoute(quoter, graph, token, WAVAX, forward.amountOut, overrides);
        if (!reverse) continue;

        const profitBps = Number((reverse.amountOut - amt.wavax) * 10000n / amt.wavax);

        if (profitBps <= 0) continue;

        // Profitable! Log it
        const hops = `${forward.route.length}→${reverse.route.length}`;
        console.log(`  [+${profitBps}bps] ${token} @ ${amt.label} (${hops} hops)`);

        // Full EVM verification via eth_call
        let verified = false;
        let verifiedProfitBps: number | undefined;
        try {
          const fwdOut = await quoteRoute(client, forward.route, amt.wavax);
          const revOut = await quoteRoute(client, reverse.route, fwdOut);
          verifiedProfitBps = Number((revOut - amt.wavax) * 10000n / amt.wavax);
          verified = verifiedProfitBps > 0;

          if (verified) {
            console.log(`    ✓ verified: +${verifiedProfitBps}bps`);
          } else {
            console.log(`    ✗ verification failed: ${verifiedProfitBps}bps`);
          }
        } catch (err: any) {
          console.log(`    ✗ verification error: ${err.message?.slice(0, 80)}`);
        }

        opportunities.push({
          token,
          amount: amt.label,
          profitBps,
          forwardHops: forward.route.length,
          reverseHops: reverse.route.length,
          verified,
          verifiedProfitBps,
        });
      } catch {
        // skip errors for individual token/amount combos
      }
    }
  }

  // 7. Summary
  console.log(`\n${"=".repeat(60)}`);
  console.log(`Scanned ${targetTokens.length} tokens × ${AMOUNTS.length} amounts = ${targetTokens.length * AMOUNTS.length} roundtrips`);
  console.log(`Found ${opportunities.length} opportunities (${opportunities.filter(o => o.verified).length} verified)\n`);

  if (opportunities.length > 0) {
    // Sort by profit descending
    opportunities.sort((a, b) => b.profitBps - a.profitBps);
    console.log("Top opportunities:");
    for (const opp of opportunities.slice(0, 10)) {
      const tag = opp.verified ? `✓ ${opp.verifiedProfitBps}bps` : "unverified";
      console.log(`  ${opp.token} @ ${opp.amount}: +${opp.profitBps}bps (${opp.forwardHops}→${opp.reverseHops} hops) [${tag}]`);
    }
  }

  // Cleanup
  quoter.close();
  closePool(STATE_SERVER_URL);
  process.exit(0);
}

main().catch(err => {
  console.error(err);
  process.exit(1);
});
