// Round-trip pathfinding benchmark
//
// For each token at 3 volume tiers (0.01, 1, 100 AVAX equivalent):
//   1. Find best route WAVAX → token, quote it
//   2. Find best route token → WAVAX, quote with the output from step 1
//   3. return_rate = amountBack / amountIn
//
// Returns per-size coverage & efficiency + average time:
//   small_coverage, small_efficiency   (0.01 AVAX)
//   med_coverage,   med_efficiency     (1 AVAX)
//   big_coverage,   big_efficiency     (100 AVAX)
//   avg_ms                             (average ms per round-trip)
//
// Agents optimize pathfinder/index.ts — this file is read-only for them.

import { loadPools } from "../../pool-collector/pools.ts";
import { buildGraph, findBestRoute } from "../index.ts";
import { createQuoter, buildStateOverrides } from "../../evm-quoter/sdk.ts";

const STATE_SERVER_URL = process.env.STATE_SERVER_URL || "ws://localhost:7449";
const POOLS_PATH = process.env.POOLS_PATH || "pool-collector/data/pools.txt";

const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";

// Benchmark tokens
const TOKENS: { address: string; label: string }[] = [
    { address: "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e", label: "USDC" },
    { address: "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7", label: "USDT" },
    { address: "0x152b9d0fdc40c096757f570a51e494bd4b943e50", label: "BTC.b" },
];

// Volume tiers in WAVAX (18 decimals)
const VOLUMES = [
    { wavax: 10n ** 16n, label: "0.01 AVAX", key: "small" },
    { wavax: 10n ** 18n, label: "1 AVAX", key: "med" },
    { wavax: 100n * 10n ** 18n, label: "100 AVAX", key: "big" },
] as const;

type SizeKey = (typeof VOLUMES)[number]["key"];

function geomean(rates: number[]): number {
    if (rates.length === 0) return 0;
    const logSum = rates.reduce((s, r) => s + Math.log(r), 0);
    return Math.round(Math.exp(logSum / rates.length) * 1000) / 10;
}

export async function run(count?: number): Promise<Record<string, number>> {
    const tokens = TOKENS.slice(0, count || TOKENS.length);

    const { pools } = loadPools(POOLS_PATH);
    const poolList = [...pools.values()].slice(0, 1000);
    const graph = buildGraph(poolList);

    // Build state overrides for all tokens we might route through
    const { overrides } = buildStateOverrides(poolList, {
        tokenAmounts: new Map([
            [WAVAX, 1000n * 10n ** 18n], // enough for 100 AVAX tier
            ...tokens.map((t) => [t.address, 10n ** 18n] as [string, bigint]),
        ]),
    });

    const quoter = await createQuoter("native", {
        stateServerUrl: STATE_SERVER_URL,
    });

    const ratesBySize: Record<SizeKey, number[]> = {
        small: [],
        med: [],
        big: [],
    };
    const totalBySize: Record<SizeKey, number> = { small: 0, med: 0, big: 0 };
    const succBySize: Record<SizeKey, number> = { small: 0, med: 0, big: 0 };
    let totalMs = 0;
    let totalRoundtrips = 0;

    for (const token of tokens) {
        for (const vol of VOLUMES) {
            totalBySize[vol.key]++;
            const tag = `${token.label} @ ${vol.label}`;
            const t0 = Date.now();

            try {
                const forward = await findBestRoute(
                    quoter,
                    graph,
                    WAVAX,
                    token.address,
                    vol.wavax,
                    overrides,
                );
                if (!forward) {
                    console.log(`  ${tag}: no forward route`);
                    totalMs += Date.now() - t0;
                    totalRoundtrips++;
                    continue;
                }

                const reverse = await findBestRoute(
                    quoter,
                    graph,
                    token.address,
                    WAVAX,
                    forward.amountOut,
                    overrides,
                );
                if (!reverse) {
                    console.log(`  ${tag}: no reverse route`);
                    totalMs += Date.now() - t0;
                    totalRoundtrips++;
                    continue;
                }

                const elapsed = Date.now() - t0;
                totalMs += elapsed;
                totalRoundtrips++;

                const rate =
                    Number((reverse.amountOut * 10000n) / vol.wavax) / 10000;
                ratesBySize[vol.key].push(rate);
                succBySize[vol.key]++;

                const pct = (rate * 100).toFixed(1);
                const hops = `${forward.route.length}→${reverse.route.length}`;
                const evmCalls = forward.stats.evmCalls + reverse.stats.evmCalls;
                const misses = forward.stats.cacheMisses + reverse.stats.cacheMisses;
                const status =
                    rate >= 0.9 ? "OK" : rate >= 0.5 ? "WEAK" : "BAD";
                console.log(
                    `  ${tag}: ${pct}% return (${hops} hops) ${elapsed}ms ${evmCalls} evm/${misses} miss [${status}]`,
                );
            } catch (err: any) {
                totalMs += Date.now() - t0;
                totalRoundtrips++;
                console.log(`  ${tag}: ERROR — ${err.message?.slice(0, 80)}`);
            }
        }
    }

    quoter.close();

    const result: Record<string, number> = {};
    for (const vol of VOLUMES) {
        const cov =
            Math.round((succBySize[vol.key] / totalBySize[vol.key]) * 1000) /
            10;
        const eff = geomean(ratesBySize[vol.key]);
        result[`${vol.key}_coverage`] = cov;
        result[`${vol.key}_efficiency`] = eff;
        console.log(`\n  ${vol.label}: coverage ${cov}%, efficiency ${eff}%`);
    }

    const avgMs = Math.round(totalMs / totalRoundtrips);
    result.avg_ms = avgMs;
    console.log(`\n  Avg time: ${avgMs}ms per round-trip`);

    return result;
}

// CLI entry point
if (
    process.argv[1] &&
    import.meta.filename?.endsWith(process.argv[1].replace(/.*\//, ""))
) {
    const count = parseInt(process.argv[2] || "0") || undefined;
    run(count)
        .then((result) => {
            for (const [k, v] of Object.entries(result)) {
                console.log(`\n>>> ${k}: ${v}`);
            }
            process.exit(0);
        })
        .catch((err) => {
            console.error(err);
            process.exit(1);
        });
}
