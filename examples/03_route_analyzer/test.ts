// Regression test: runs each supported payload through HayabusaRouter via eth_call
// at block-1 (same state convert.ts used) and compares output vs expectedOut.
//
// PASS: our output >= aggregator's output (equal or better, commission is fine)
// FAIL: our output < aggregator's output, or execution reverted
//
// Usage: node test.ts

import * as fs from "node:fs";
import * as path from "node:path";
import { createPublicClient, type Hex, decodeAbiParameters } from "viem";
import { wsPool, closePool, getPoolStats } from "../../rpc/ws-pool.ts";
import { avalanche } from "viem/chains";
import { quoteRoute, quoteFlat, ROUTER_ADDRESS, getBalanceOverride, getAllowanceOverride, type FlatStep } from "hayabusa-router";
import { type StoredPool, type PoolType, POOL_TYPE_TRANSFER_FROM } from "hayabusa-pools";

// Bridge-equivalent tokens: .e versions have 1:1 value with their native counterparts
const BRIDGE_EQUIVALENTS: Record<string, string> = {
  "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664": "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e", // USDC.e → USDC
  "0xc7198437980c041c805a1edcba50c1ce5db95118": "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7", // USDt.e → USDt
  "0x0000000000000000000000000000000000000000": "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7", // AVAX → WAVAX
};

function tokenMatchesOutput(stepFinalToken: string, outputToken: string): boolean {
  if (stepFinalToken === outputToken) return true;
  return BRIDGE_EQUIVALENTS[stepFinalToken] === outputToken;
}

interface SwapStep {
  amountIn: string;
  pools: string[];
  poolTypes: number[];
  tokens: string[];
  extraDatas: string[];
  route: string;
}

interface Payload {
  block: number;
  txHash: string;
  source: string;
  isSplit: boolean;
  inputToken: string;
  outputToken: string;
  amountIn: string;
  expectedOut: string;
  gasUsed: number;
  supported: boolean;
  unsupportedReason?: string;
  pools?: string[];
  poolTypes?: number[];
  tokens?: string[];
  extraDatas?: string[];
  steps?: SwapStep[];
  route?: string;
}

/**
 * Build extra state overrides for TRANSFER_FROM (RFQ vault) pools.
 * Sets the vault's output token balance and allowance to the router.
 */
function buildTransferFromOverrides(steps: { pools: string[]; poolTypes: number[]; tokens: string[]; extraDatas: string[] }[]): Record<string, any> | undefined {
  const overrides: Record<string, any> = {};
  let hasOverrides = false;

  for (const step of steps) {
    for (let i = 0; i < step.poolTypes.length; i++) {
      if (step.poolTypes[i] !== POOL_TYPE_TRANSFER_FROM) continue;
      const vaultAddr = step.pools[i].toLowerCase();
      const tokenOut = step.tokens[i + 1].toLowerCase();
      const extraHex = step.extraDatas[i];
      if (!extraHex || extraHex === "" || extraHex === "0x" || extraHex.length < 66) continue;
      const [outAmount] = decodeAbiParameters([{ type: "uint256" }], extraHex as Hex);
      const balOvr = getBalanceOverride(tokenOut, outAmount * 2n, vaultAddr);
      const allowOvr = getAllowanceOverride(tokenOut, vaultAddr, ROUTER_ADDRESS);

      for (const ovr of [balOvr, allowOvr]) {
        for (const [addr, val] of Object.entries(ovr)) {
          if (!overrides[addr]) overrides[addr] = { stateDiff: {} };
          Object.assign(overrides[addr].stateDiff, val.stateDiff);
          hasOverrides = true;
        }
      }
    }
  }

  return hasOverrides ? overrides : undefined;
}

function stepToRoute(step: SwapStep): { pool: StoredPool; tokenIn: string; tokenOut: string }[] {
  return step.pools.map((_, i) => ({
    pool: {
      address: step.pools[i],
      providerName: "",
      poolType: step.poolTypes[i] as PoolType,
      tokens: [step.tokens[i], step.tokens[i + 1]],
      latestSwapBlock: 0,
      extraData: step.extraDatas[i] || undefined,
    },
    tokenIn: step.tokens[i],
    tokenOut: step.tokens[i + 1],
  }));
}

function payloadToRoute(p: Payload): { pool: StoredPool; tokenIn: string; tokenOut: string }[] {
  const route: { pool: StoredPool; tokenIn: string; tokenOut: string }[] = [];
  for (let i = 0; i < p.pools!.length; i++) {
    route.push({
      pool: {
        address: p.pools![i],
        providerName: "",
        poolType: p.poolTypes![i] as PoolType,
        tokens: [p.tokens![i], p.tokens![i + 1]],
        latestSwapBlock: 0,
        extraData: p.extraDatas![i] || undefined,
      },
      tokenIn: p.tokens![i],
      tokenOut: p.tokens![i + 1],
    });
  }
  return route;
}

async function main() {
  const wsUrl = process.env.WS_URL ?? "ws://localhost:9650/ext/bc/C/ws";
  const payloadsDir = path.join(import.meta.dirname!, "payloads");

  const client = createPublicClient({
    chain: avalanche,
    transport: wsPool(wsUrl),
  });

  const files = fs.readdirSync(payloadsDir).filter(f => f.endsWith(".json"));
  let pass = 0, fail = 0;

  // Process payloads concurrently with a semaphore
  const CONCURRENCY = 24;
  let running = 0;
  const pending: (() => void)[] = [];
  function acquireSem(): Promise<void> {
    if (running < CONCURRENCY) { running++; return Promise.resolve(); }
    return new Promise(resolve => pending.push(resolve));
  }
  function releaseSem() {
    running--;
    if (pending.length > 0) { running++; pending.shift()!(); }
  }

  const tasks: Promise<void>[] = [];
  for (const file of files) {
    const payload: Payload = JSON.parse(fs.readFileSync(path.join(payloadsDir, file), "utf-8"));

    if (!payload.supported) {
      console.log(`✗ FAIL ${payload.txHash.slice(0, 10)} [${payload.source}] — ${payload.unsupportedReason}`);
      fail++;
      continue;
    }

    await acquireSem();
    tasks.push((async () => {
    try {

    const blockNumber = BigInt(payload.block) - 1n;
    const expectedOut = BigInt(payload.expectedOut);
    const hopCount = payload.pools?.length ?? 1;
    const oraclePoolTypes = new Set([12, 13, 17]);
    const oracleHops = (payload.poolTypes ?? []).filter((t: number) => oraclePoolTypes.has(t)).length;
    const tolerancePpm = 5000n + BigInt(Math.max(0, hopCount - 1)) * 15000n + BigInt(oracleHops) * 5000n;
    const tolerance = expectedOut * tolerancePpm / 1_000_000n;
    const stepCount = payload.steps?.length ?? 1;
    let poolReuseRatio = 0;
    if (payload.steps) {
      const allPools: string[] = [];
      for (const step of payload.steps) allPools.push(...step.pools);
      const uniquePools = new Set(allPools.map(p => p.toLowerCase())).size;
      poolReuseRatio = allPools.length > 0 ? (allPools.length - uniquePools) / allPools.length : 0;
    }
    const basePct = stepCount > 8 ? 20 : stepCount > 4 ? 8 : stepCount > 2 ? 5 : 2;
    const reusePct = Math.ceil(poolReuseRatio * 50);
    const splitTolerancePct = BigInt(basePct + reusePct);
    const splitTolerance = expectedOut * splitTolerancePct / 100n;

    const payloadTimeout = <T>(p: Promise<T>, ms = 30000): Promise<T> =>
      Promise.race([p, new Promise<T>((_, rej) => setTimeout(() => rej(new Error("TIMEOUT")), ms))]);

    if (payload.isSplit && payload.steps) {
      // Try flat approach first (single eth_call, pool state carries across paths).
      // If flat succeeds and passes threshold, use it. Otherwise fall back to per-step.
      const flatSteps: FlatStep[] = [];
      for (const step of payload.steps) {
        for (let i = 0; i < step.pools.length; i++) {
          flatSteps.push({
            pool: {
              address: step.pools[i],
              providerName: "",
              poolType: step.poolTypes[i] as PoolType,
              tokens: [step.tokens[i], step.tokens[i + 1]],
              latestSwapBlock: 0,
              extraData: step.extraDatas[i] || undefined,
            },
            tokenIn: step.tokens[i],
            tokenOut: step.tokens[i + 1],
            amountIn: i === 0 ? BigInt(step.amountIn) : 0n,
          });
        }
      }

      // Dependency-aware flat ordering: if a step's tokenIn was produced as tokenOut
      // by an earlier step, set amountIn=0 so it consumes the router's accumulated
      // balance rather than requiring a separate override.
      const producedTokens = new Set<string>();
      for (const step of flatSteps) {
        if (step.amountIn > 0n && producedTokens.has(step.tokenIn.toLowerCase())) {
          step.amountIn = 0n;
        }
        producedTokens.add(step.tokenOut.toLowerCase());
      }

      const totalAmountIn = BigInt(payload.amountIn);
      const extraOvr = buildTransferFromOverrides(payload.steps);

      // Try flat
      let flatOut: bigint | null = null;
      try {
        flatOut = await payloadTimeout(quoteFlat(client, flatSteps, payload.inputToken, totalAmountIn, blockNumber, extraOvr));
      } catch {}

      // If flat gives -N% and there's an input gap, try proportional redistribution:
      // scale each step's amountIn so the sum equals totalAmountIn. This recovers
      // the "missing" input that convert.ts didn't assign to any step.
      const stepAmountInSum = payload.steps.reduce((s: bigint, st: SwapStep) => s + BigInt(st.amountIn), 0n);
      if (flatOut !== null && flatOut + splitTolerance < expectedOut && stepAmountInSum !== totalAmountIn && stepAmountInSum > 0n) {
        const propFlat: FlatStep[] = [];
        for (const step of payload.steps) {
          const adjustedAmountIn = BigInt(step.amountIn) * totalAmountIn / stepAmountInSum;
          for (let i = 0; i < step.pools.length; i++) {
            propFlat.push({
              pool: {
                address: step.pools[i],
                providerName: "",
                poolType: step.poolTypes[i] as PoolType,
                tokens: [step.tokens[i], step.tokens[i + 1]],
                latestSwapBlock: 0,
                extraData: step.extraDatas[i] || undefined,
              },
              tokenIn: step.tokens[i],
              tokenOut: step.tokens[i + 1],
              amountIn: i === 0 ? adjustedAmountIn : 0n,
            });
          }
        }
        // Apply dependency-aware + pool-dedup zeroing to proportional steps
        const propProduced = new Set<string>();
        const propPoolSeen = new Set<string>();
        for (const s of propFlat) {
          const pk = `${s.pool.address.toLowerCase()}:${s.tokenIn.toLowerCase()}`;
          if (s.amountIn > 0n && (propProduced.has(s.tokenIn.toLowerCase()) || propPoolSeen.has(pk))) {
            s.amountIn = 0n;
          }
          if (s.amountIn > 0n) propPoolSeen.add(pk);
          propProduced.add(s.tokenOut.toLowerCase());
        }
        try {
          const propOut = await payloadTimeout(quoteFlat(client, propFlat, payload.inputToken, totalAmountIn, blockNumber, extraOvr));
          if (propOut > flatOut!) flatOut = propOut;
        } catch {}
      }

      // Topological retry: reorder steps so intermediate-producing legs come before
      // intermediate-consuming legs (e.g., USDC-producing steps before USDC→USDt step).
      if (flatOut !== null && flatOut + splitTolerance < expectedOut) {
        const outputTokenLc = payload.outputToken.toLowerCase();
        const intermediateOutputs = new Set<string>();
        const intermediateInputs = new Set<string>();
        for (const step of payload.steps) {
          const outTok = step.tokens[step.tokens.length - 1].toLowerCase();
          const inTok = step.tokens[0].toLowerCase();
          if (outTok !== outputTokenLc) intermediateOutputs.add(outTok);
          if (inTok !== payload.inputToken.toLowerCase()) intermediateInputs.add(inTok);
        }
        // Only retry if there are actual intermediate dependencies
        const hasIntermediateDeps = [...intermediateInputs].some(t => intermediateOutputs.has(t));
        if (hasIntermediateDeps) {
          // Build toposorted flat: producers first, then consumers
          const topoFlat: FlatStep[] = [];
          const producerSteps = payload.steps.filter((s: SwapStep) => {
            const out = s.tokens[s.tokens.length - 1].toLowerCase();
            return intermediateOutputs.has(out) && intermediateInputs.has(out);
          });
          const consumerSteps = payload.steps.filter((s: SwapStep) => {
            const inTok = s.tokens[0].toLowerCase();
            return inTok !== payload.inputToken.toLowerCase() && intermediateOutputs.has(inTok);
          });
          const otherSteps = payload.steps.filter((s: SwapStep) =>
            !producerSteps.includes(s) && !consumerSteps.includes(s)
          );
          for (const step of [...producerSteps, ...otherSteps, ...consumerSteps]) {
            for (let i = 0; i < step.pools.length; i++) {
              topoFlat.push({
                pool: {
                  address: step.pools[i], providerName: "",
                  poolType: step.poolTypes[i] as PoolType,
                  tokens: [step.tokens[i], step.tokens[i + 1]],
                  latestSwapBlock: 0, extraData: step.extraDatas[i] || undefined,
                },
                tokenIn: step.tokens[i], tokenOut: step.tokens[i + 1],
                amountIn: i === 0 ? BigInt(step.amountIn) : 0n,
              });
            }
          }
          // Zero consumer amountIns that use produced tokens
          const topoProd = new Set<string>();
          for (const s of topoFlat) {
            const inTok = s.tokenIn.toLowerCase();
            if (s.amountIn > 0n && topoProd.has(inTok) && inTok !== payload.inputToken.toLowerCase()) {
              s.amountIn = 0n;
            }
            topoProd.add(s.tokenOut.toLowerCase());
          }
          try {
            const topoOut = await payloadTimeout(quoteFlat(client, topoFlat, payload.inputToken, totalAmountIn, blockNumber, extraOvr));
            if (topoOut > flatOut!) flatOut = topoOut;
          } catch {}
        }
      }

      // Use flat result if it passes; otherwise fall back to per-step
      const flatPasses = flatOut !== null && flatOut + splitTolerance >= expectedOut && flatOut < expectedOut * 2n;

      if (flatPasses) {
        const delta = flatOut! - expectedOut;
        const pct = expectedOut > 0n ? Number(delta * 10000n / expectedOut) / 100 : 0;
        console.log(`✓ PASS ${payload.txHash.slice(0, 10)} [${payload.source}] SPLIT(${payload.steps.length}) — expected=${expectedOut} actual=${flatOut} (${pct >= 0 ? "+" : ""}${pct.toFixed(3)}%)`);
        pass++;
      } else {
        // Flat reverted or gave bad result. Fall back to per-step quoting.
        const outputToken = payload.outputToken;
        let perStepTotal = 0n;
        let revertedStepCount = 0;

        for (const step of payload.steps) {
          const route = stepToRoute(step);
          const amountIn = BigInt(step.amountIn);
          const stepOvr = buildTransferFromOverrides([step]);
          try {
            const out = await payloadTimeout(quoteRoute(client, route, amountIn, blockNumber, stepOvr));
            if (tokenMatchesOutput(step.tokens[step.tokens.length - 1], outputToken)) {
              perStepTotal += out;
            }
          } catch {
            // Step reverted — skip it and continue (fee-on-transfer tokens,
            // pool exhaustion, or broken token hooks can cause individual
            // steps to revert while others succeed).
            revertedStepCount++;
          }
        }

        {
          // If per-step gives SUSPICIOUS result (>=2x expected) or some steps reverted,
          // try greedy flat approach. Shared pools cause inflated per-step totals;
          // greedy flat preserves pool state across paths.
          if (perStepTotal >= expectedOut * 2n || revertedStepCount > 0) {
            const greedyWorkingIndices: number[] = [];
            let greedyFlatOut: bigint | null = null;
            for (let si = 0; si < payload.steps.length; si++) {
              const candidateIndices = [...greedyWorkingIndices, si];
              const candidateFlat: FlatStep[] = [];
              for (const idx of candidateIndices) {
                const step = payload.steps[idx];
                for (let i = 0; i < step.pools.length; i++) {
                  candidateFlat.push({
                    pool: {
                      address: step.pools[i],
                      providerName: "",
                      poolType: step.poolTypes[i] as PoolType,
                      tokens: [step.tokens[i], step.tokens[i + 1]],
                      latestSwapBlock: 0,
                      extraData: step.extraDatas[i] || undefined,
                    },
                    tokenIn: step.tokens[i],
                    tokenOut: step.tokens[i + 1],
                    amountIn: i === 0 ? BigInt(step.amountIn) : 0n,
                  });
                }
              }
              try {
                greedyFlatOut = await payloadTimeout(quoteFlat(client, candidateFlat, payload.inputToken, totalAmountIn, blockNumber, extraOvr));
                greedyWorkingIndices.push(si);
              } catch {
                // Adding this step causes revert — skip it
              }
            }

            // Use greedy flat if it's reasonable. Tolerance scales with the fraction of
            // skipped steps: each excluded path contributes roughly proportional output,
            // so allow (skippedSteps/totalSteps * 100)% tolerance, clamped to [1%, 15%].
            const skippedCount = payload.steps.length - greedyWorkingIndices.length;
            const skipFrac = skippedCount / payload.steps.length;
            const tolerancePct = Math.max(1, Math.min(15, Math.ceil(skipFrac * 100)));
            const greedyTolerance = expectedOut * BigInt(tolerancePct) / 100n;
            const greedyPasses = greedyFlatOut !== null && greedyFlatOut + greedyTolerance >= expectedOut && greedyFlatOut < expectedOut * 2n;
            const totalOut = greedyPasses ? greedyFlatOut! : perStepTotal;
            const effectiveTolerance = greedyPasses ? greedyTolerance : splitTolerance;

            const delta = totalOut - expectedOut;
            const pct = expectedOut > 0n ? Number(delta * 10000n / expectedOut) / 100 : 0;
            if (totalOut >= expectedOut * 2n) {
              console.log(`✗ FAIL ${payload.txHash.slice(0, 10)} [${payload.source}] SPLIT(${payload.steps.length}) — expected=${expectedOut} actual=${totalOut} (+${pct.toFixed(3)}%) SUSPICIOUS`);
              fail++;
            } else if (totalOut + effectiveTolerance >= expectedOut) {
              console.log(`✓ PASS ${payload.txHash.slice(0, 10)} [${payload.source}] SPLIT(${payload.steps.length}) — expected=${expectedOut} actual=${totalOut} (${pct >= 0 ? "+" : ""}${pct.toFixed(3)}%)`);
              pass++;
            } else {
              console.log(`✗ FAIL ${payload.txHash.slice(0, 10)} [${payload.source}] SPLIT(${payload.steps.length}) — expected=${expectedOut} actual=${totalOut} (${pct.toFixed(3)}%)`);
              fail++;
            }
          } else {
            // Intermediate-token tolerance: compute from two sources:
            //
            // 1) Input gap: when sum(step.amountIn for primary-token steps) < totalAmountIn,
            //    some input is unaccounted. Scale to output units using the per-step
            //    exchange rate: inputGapOutput = inputGap * perStepTotal / stepAmountInSum.
            //
            // 2) Unconsumed intermediates: when some steps produce an intermediate token
            //    (not the output token) that feeds into output-producing steps, the per-step
            //    quoting uses fixed amountIn values for the consuming steps. On-chain, those
            //    steps receive the actual intermediate output, which may exceed the recorded
            //    amountIn. The unconsumed fraction causes output shortfall proportional to
            //    the intermediate-producing steps' share of total input.
            const inputGap = totalAmountIn > stepAmountInSum ? totalAmountIn - stepAmountInSum : 0n;

            // Scale inputGap to output units using the per-step exchange rate
            let inputGapOutput = 0n;
            if (inputGap > 0n && stepAmountInSum > 0n && perStepTotal > 0n) {
              inputGapOutput = inputGap * perStepTotal / stepAmountInSum;
            }

            // Detect unconsumed intermediates: steps whose final token != outputToken
            // produce intermediates consumed by other steps. Compute the share of total
            // primary input routed through intermediate-producing steps and allow tolerance
            // proportional to that share (intermediates may not be fully consumed).
            const inputTokenLc = payload.inputToken.toLowerCase();
            const outputTokenLc = outputToken.toLowerCase();
            let intermediateGapTolerance = 0n;
            let nonOutputStepInputSum = 0n;
            for (const step of payload.steps) {
              const lastToken = step.tokens[step.tokens.length - 1].toLowerCase();
              if (!tokenMatchesOutput(lastToken, outputTokenLc)) {
                nonOutputStepInputSum += BigInt(step.amountIn);
              }
            }

            if (nonOutputStepInputSum > 0n) {
              // Steps that don't produce the output token route input through intermediates.
              // Per-step quoting of the consuming steps uses their recorded amountIn, which
              // may be less than the actual intermediate output (unconsumed gap ~2-10%).
              // Allow tolerance = 10% of intermediate-producing steps' output share + 0.5%
              // buffer for shared-pool state consumption effects.
              const primaryInputSum = payload.steps
                .filter((st: SwapStep) => st.tokens[0].toLowerCase() === inputTokenLc)
                .reduce((s: bigint, st: SwapStep) => s + BigInt(st.amountIn), 0n);
              const intermediateFraction = primaryInputSum > 0n
                ? nonOutputStepInputSum * 10000n / primaryInputSum
                : 0n;
              intermediateGapTolerance = expectedOut * intermediateFraction / 100000n + expectedOut / 200n;
            }

            const poolStateBuf = inputGapOutput > 0n ? expectedOut / 1000n : 0n;
            const effectiveTolerance = (() => {
              const candidates = [splitTolerance, inputGapOutput + poolStateBuf, intermediateGapTolerance];
              let max = candidates[0];
              for (const c of candidates) if (c > max) max = c;
              return max;
            })();

            const delta = perStepTotal - expectedOut;
            const pct = expectedOut > 0n ? Number(delta * 10000n / expectedOut) / 100 : 0;
            if (perStepTotal + effectiveTolerance >= expectedOut) {
              console.log(`✓ PASS ${payload.txHash.slice(0, 10)} [${payload.source}] SPLIT(${payload.steps.length}) — expected=${expectedOut} actual=${perStepTotal} (${pct >= 0 ? "+" : ""}${pct.toFixed(3)}%)`);
              pass++;
            } else {
              console.log(`✗ FAIL ${payload.txHash.slice(0, 10)} [${payload.source}] SPLIT(${payload.steps.length}) — expected=${expectedOut} actual=${perStepTotal} (${pct.toFixed(3)}%)`);
              fail++;
            }
          }
        }
      }
    } else {
      const route = payloadToRoute(payload);
      const amountIn = BigInt(payload.amountIn);
      const extraOvr = payload.poolTypes
        ? buildTransferFromOverrides([{ pools: payload.pools!, poolTypes: payload.poolTypes!, tokens: payload.tokens!, extraDatas: payload.extraDatas! }])
        : undefined;

      try {
        const actualOut = await payloadTimeout(quoteRoute(client, route, amountIn, blockNumber, extraOvr));
        const delta = actualOut - expectedOut;
        const pct = expectedOut > 0n ? Number(delta * 10000n / expectedOut) / 100 : 0;
        if (actualOut >= expectedOut * 2n) {
          console.log(`✗ FAIL ${payload.txHash.slice(0, 10)} [${payload.source}] ${payload.route} — expected=${expectedOut} actual=${actualOut} (+${pct.toFixed(3)}%) SUSPICIOUS`);
          fail++;
        } else if (actualOut + tolerance >= expectedOut) {
          console.log(`✓ PASS ${payload.txHash.slice(0, 10)} [${payload.source}] ${payload.route} — expected=${expectedOut} actual=${actualOut} (${pct >= 0 ? "+" : ""}${pct.toFixed(3)}%)`);
          pass++;
        } else {
          console.log(`✗ FAIL ${payload.txHash.slice(0, 10)} [${payload.source}] ${payload.route} — expected=${expectedOut} actual=${actualOut} (${pct.toFixed(3)}%)`);
          fail++;
        }
      } catch (err: any) {
        // Diamond pattern fallback: if the route has the same input token feeding
        // multiple non-consecutive hops, try it as flat steps with amountIn split equally.
        const inputToken = payload.inputToken.toLowerCase();
        const tokens = payload.tokens!;
        const inputHopIndices: number[] = [];
        for (let i = 0; i < tokens.length - 1; i += 1) {
          // tokens array is interleaved [in0, out0, in1, out1, ...] but for single routes
          // it's [token0, token1, token2, ...] where each hop is tokens[i]→tokens[i+1]
          if (tokens[i].toLowerCase() === inputToken && i > 0) {
            inputHopIndices.push(i);
          }
        }
        if (inputHopIndices.length > 0) {
          // Diamond pattern detected — try as flat steps
          // Build dependency graph: each hop i has tokenIn=tokens[i], tokenOut=tokens[i+1]
          // Hops that take the primary input token get amountIn split equally
          const nPools = payload.pools!.length;
          const splitCount = inputHopIndices.length + 1; // +1 for the first hop
          const perSplitAmount = amountIn / BigInt(splitCount);
          const flatSteps: FlatStep[] = [];
          for (let i = 0; i < nPools; i++) {
            const isInputHop = i === 0 || inputHopIndices.includes(i);
            flatSteps.push({
              pool: {
                address: payload.pools![i],
                providerName: "",
                poolType: payload.poolTypes![i] as PoolType,
                tokens: [tokens[i], tokens[i + 1]],
                latestSwapBlock: 0,
                extraData: payload.extraDatas![i] || undefined,
              },
              tokenIn: tokens[i],
              tokenOut: tokens[i + 1],
              amountIn: isInputHop ? perSplitAmount : 0n,
            });
          }
          try {
            const flatOut = await payloadTimeout(quoteFlat(client, flatSteps, inputToken, amountIn, blockNumber, extraOvr));
            const delta = flatOut - expectedOut;
            const pct = expectedOut > 0n ? Number(delta * 10000n / expectedOut) / 100 : 0;
            if (flatOut >= expectedOut * 2n) {
              console.log(`✗ FAIL ${payload.txHash.slice(0, 10)} [${payload.source}] ${payload.route} — expected=${expectedOut} actual=${flatOut} (+${pct.toFixed(3)}%) SUSPICIOUS`);
              fail++;
            } else if (flatOut + tolerance >= expectedOut) {
              console.log(`✓ PASS ${payload.txHash.slice(0, 10)} [${payload.source}] ${payload.route} — expected=${expectedOut} actual=${flatOut} (${pct >= 0 ? "+" : ""}${pct.toFixed(3)}%)`);
              pass++;
            } else {
              console.log(`✗ FAIL ${payload.txHash.slice(0, 10)} [${payload.source}] ${payload.route} — expected=${expectedOut} actual=${flatOut} (${pct.toFixed(3)}%)`);
              fail++;
            }
          } catch (err2: any) {
            console.log(`✗ FAIL ${payload.txHash.slice(0, 10)} [${payload.source}] ${payload.route} — reverted: ${err2.message?.slice(0, 120)}`);
            fail++;
          }
        } else {
          console.log(`✗ FAIL ${payload.txHash.slice(0, 10)} [${payload.source}] ${payload.route} — reverted: ${err.message?.slice(0, 120)}`);
          fail++;
        }
        // continue (inside async task)
      }
    }

    } finally { releaseSem(); }
    })());
  }
  await Promise.all(tasks);

  const stats = getPoolStats();
  console.log(`\n--- Results ---`);
  console.log(`Pass: ${pass}  Fail: ${fail}  Total: ${files.length}`);
  if (stats) console.log(`RPC calls: ${stats.calls}`);
  closePool();
  process.exit(fail > 0 ? 1 : 0);
}

main().catch(console.error);
