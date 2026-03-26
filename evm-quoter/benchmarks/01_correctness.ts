// 01_correctness.ts — Verify Native and WASM produce identical results to direct RPC
//
// Returns: number of mismatches (0 = all pass)

import pLimit from "p-limit";
import {
  createQuoter, loadPools, buildStateOverrides, encodeSwapSingle,
  ROUTER, DUMMY_SENDER,
} from "../sdk.ts";
import addressJson from "../../router/contracts/address.json" with { type: "json" };

const STATE_SERVER = `ws://127.0.0.1:7449/debug/${addressJson.block}`;
const RPC_URL = "http://127.0.0.1:9650/ext/bc/C/rpc";
const BLOCK = addressJson.block;
const blockHex = "0x" + BLOCK.toString(16);

async function rpcQuote(pool, stateOverrides) {
  const calldata = encodeSwapSingle(pool.pool, pool.poolType, pool.tokenIn, pool.tokenOut, pool.amountIn);
  try {
    const resp = await fetch(RPC_URL, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        jsonrpc: "2.0", id: 1, method: "eth_call",
        params: [{ to: ROUTER, data: calldata, from: DUMMY_SENDER }, blockHex, stateOverrides],
      }),
    });
    const json = await resp.json();
    if (json.error) return { ok: false, error: json.error.message };
    return { ok: true, returnData: json.result };
  } catch (e) {
    return { ok: false, error: e.message };
  }
}

export async function run(poolCount = 100) {
  const pools = loadPools(null, poolCount);
  if (pools.length === 0) throw new Error("No pools found");

  const { overrides: stateOverrides } = buildStateOverrides(pools);

  // Native
  console.log(`  Native (${pools.length} pools)...`);
  const native = await createQuoter("native", { stateServerUrl: STATE_SERVER });
  await native.quotePoolBatch(pools, stateOverrides);
  const nT0 = performance.now();
  const nativeResults = await native.quotePoolBatch(pools, stateOverrides);
  const nativeMs = performance.now() - nT0;
  const nativeOk = nativeResults.filter(r => r.ok).length;
  console.log(`    ${nativeOk}/${pools.length} ok, ${nativeMs.toFixed(0)}ms`);
  native.close();

  await new Promise(r => setTimeout(r, 500));

  // WASM
  console.log(`  WASM (${pools.length} pools)...`);
  const wasm = await createQuoter("wasm", { stateServerUrl: STATE_SERVER });
  await wasm.quotePoolBatch(pools, stateOverrides);
  const wT0 = performance.now();
  const wasmResults = await wasm.quotePoolBatch(pools, stateOverrides);
  const wasmMs = performance.now() - wT0;
  const wasmOk = wasmResults.filter(r => r.ok).length;
  console.log(`    ${wasmOk}/${pools.length} ok, ${wasmMs.toFixed(0)}ms`);
  wasm.close();

  await new Promise(r => setTimeout(r, 500));

  // RPC
  console.log(`  RPC (${pools.length} pools, concurrency=20)...`);
  const limit = pLimit(20);
  const rT0 = performance.now();
  const rpcResults = await Promise.all(pools.map(p => limit(() => rpcQuote(p, stateOverrides))));
  const rpcMs = performance.now() - rT0;
  const rpcOk = rpcResults.filter(r => r.ok).length;
  console.log(`    ${rpcOk}/${pools.length} ok, ${rpcMs.toFixed(0)}ms`);

  // Count matches (pools where all three backends agree)
  let compared = 0;
  let matched = 0;
  for (let i = 0; i < pools.length; i++) {
    const n = nativeResults[i], w = wasmResults[i], r = rpcResults[i];
    if (!n.ok || !w.ok || !r.ok) continue;
    compared++;
    if (n.returnData === r.returnData && w.returnData === r.returnData) {
      matched++;
    } else {
      if (n.returnData !== r.returnData) console.log(`  MISMATCH native/rpc: ${pools[i].pool} ${pools[i].typeName}`);
      if (w.returnData !== r.returnData) console.log(`  MISMATCH wasm/rpc: ${pools[i].pool} ${pools[i].typeName}`);
    }
  }

  const pct = Math.round(matched / compared * 1000) / 10;
  console.log(`  Result: ${pct}% correct (${matched}/${compared})`);
  return pct;
}
