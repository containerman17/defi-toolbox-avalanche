// 02_speed.mjs — Benchmark quoting speed
//
// Returns: ms/pool for native backend

import { createQuoter, loadPools, buildStateOverrides } from "../sdk.mjs";

const STATE_SERVER = "ws://127.0.0.1:7449";

export async function run(poolCount = 1000) {
  const pools = loadPools(null, poolCount);
  if (pools.length === 0) throw new Error("No pools found");

  const { overrides: stateOverrides } = buildStateOverrides(pools);

  // Native
  console.log(`  Native (${pools.length} pools)...`);
  const native = await createQuoter("native", { stateServerUrl: STATE_SERVER });
  await native.quotePoolBatch(pools, stateOverrides); // warm
  const t0 = performance.now();
  const results = await native.quotePoolBatch(pools, stateOverrides);
  const ms = performance.now() - t0;
  const ok = results.filter(r => r.ok).length;
  native.close();

  const msPerPool = +(ms / pools.length).toFixed(3);
  console.log(`    ${ok}/${pools.length} ok, ${ms.toFixed(0)}ms total, ${msPerPool}ms/pool`);

  // WASM for comparison
  console.log(`  WASM (${pools.length} pools)...`);
  const wasm = await createQuoter("wasm", { stateServerUrl: STATE_SERVER });
  await wasm.quotePoolBatch(pools, stateOverrides);
  const wT0 = performance.now();
  await wasm.quotePoolBatch(pools, stateOverrides);
  const wasmMs = performance.now() - wT0;
  const wasmPerPool = +(wasmMs / pools.length).toFixed(3);
  console.log(`    ${wasmMs.toFixed(0)}ms total, ${wasmPerPool}ms/pool`);
  console.log(`    WASM is ${(wasmMs / ms).toFixed(1)}x slower`);
  wasm.close();

  console.log(`  Result: ${msPerPool}ms/pool`);
  return msPerPool;
}
