// 03_coverage.mjs — How many pools can we actually quote?
//
// Returns: coverage percentage (0-100)

import { createQuoter, loadPools, buildStateOverrides, decodeSwapResult } from "../sdk.mjs";

const STATE_SERVER = "ws://127.0.0.1:7449";

export async function run(poolCount = 5000) {
  const pools = loadPools(null, poolCount);
  if (pools.length === 0) throw new Error("No pools found");

  const { overrides: stateOverrides } = buildStateOverrides(pools);

  console.log(`  Quoting ${pools.length} pools...`);
  const quoter = await createQuoter("native", { stateServerUrl: STATE_SERVER });
  const results = await quoter.quotePoolBatch(pools, stateOverrides);
  quoter.close();

  // Count by type
  const byType = new Map();
  let totalOk = 0;

  for (let i = 0; i < pools.length; i++) {
    const p = pools[i];
    if (!byType.has(p.typeName)) byType.set(p.typeName, { ok: 0, total: 0 });
    const stats = byType.get(p.typeName);
    stats.total++;

    if (results[i].ok) {
      try {
        if (decodeSwapResult(results[i].returnData) > 0n) { totalOk++; stats.ok++; }
      } catch {}
    }
  }

  // Log breakdown
  const sorted = [...byType.entries()].sort((a, b) => b[1].total - a[1].total);
  for (const [name, s] of sorted) {
    const pct = ((s.ok / s.total) * 100).toFixed(0);
    console.log(`    ${name.padEnd(16)} ${s.ok}/${s.total} (${pct}%)`);
  }

  const pct = +((totalOk / pools.length) * 100).toFixed(1);
  console.log(`  Result: ${totalOk}/${pools.length} = ${pct}%`);
  return pct;
}
