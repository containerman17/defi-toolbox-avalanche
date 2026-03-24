// Round-trip pathfinding benchmark
//
// Uses Go find_route for all quoting — no JS BFS, no quotePoolBatch IPC.
//
// For each token at 3 volume tiers (0.01, 1, 100 AVAX equivalent):
//   1. find_route WAVAX → token
//   2. find_route token → WAVAX with output from step 1
//   3. return_rate = amountBack / amountIn
//
// Returns per-size coverage & efficiency + average time.

import { readFileSync } from "node:fs";
import { spawn } from "node:child_process";
import { join } from "node:path";
import { buildStateOverrides } from "../../router/overrides.ts";
import { parsePools } from "../../pool-collector/pools.ts";

const STATE_SERVER_URL = process.env.STATE_SERVER_URL || "ws://localhost:7449";
const POOLS_PATH = process.env.POOLS_PATH || "pool-collector/data/pools.txt";
const REGISTRY_PATH = "evm-quoter/go/formulas/registry.txt";
const HARNESS_PATH = "evm-quoter/bin/harness-native";

const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const ROUTER = "0x000000000000000000000000cafebabe00facade";

const TOKENS: { address: string; label: string }[] = [
    { address: "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e", label: "USDC" },
    { address: "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7", label: "USDT" },
    { address: "0x152b9d0fdc40c096757f570a51e494bd4b943e50", label: "BTC.b" },
];

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

// Harness wrapper — spawns native binary, sends find_route requests
class Harness {
    private child: ReturnType<typeof spawn>;
    private nextId = 0;
    private pending = new Map<number, (resp: any) => void>();
    private buffer = "";

    constructor() {
        this.child = spawn(HARNESS_PATH, [
            "--state-server", STATE_SERVER_URL,
            "--registry", REGISTRY_PATH,
        ], { stdio: ["pipe", "pipe", "inherit"] });

        this.child.stdout!.on("data", (chunk: Buffer) => {
            this.buffer += chunk.toString();
            let nl;
            while ((nl = this.buffer.indexOf("\n")) !== -1) {
                const line = this.buffer.slice(0, nl).trim();
                this.buffer = this.buffer.slice(nl + 1);
                if (!line) continue;
                try {
                    const resp = JSON.parse(line);
                    const resolve = this.pending.get(resp.id);
                    if (resolve) {
                        this.pending.delete(resp.id);
                        resolve(resp);
                    }
                } catch {}
            }
        });
    }

    async waitReady(): Promise<void> {
        await new Promise(r => setTimeout(r, 2500));
    }

    findRoute(tokenIn: string, tokenOut: string, amountIn: bigint, poolsContent: string, poolLimit: number, overrides: any): Promise<any> {
        const id = ++this.nextId;
        return new Promise((resolve) => {
            this.pending.set(id, resolve);
            const req = JSON.stringify({
                id,
                method: "find_route",
                params: {
                    tokenIn,
                    tokenOut,
                    amountIn: "0x" + amountIn.toString(16),
                    pools: poolsContent,
                    poolLimit,
                    stateOverrides: overrides,
                },
            }) + "\n";
            this.child.stdin!.write(req);
        });
    }

    close() {
        this.child.kill();
    }
}

export async function run(count?: number): Promise<Record<string, number>> {
    const tokens = TOKENS.slice(0, count || TOKENS.length);
    const poolsContent = readFileSync(POOLS_PATH, "utf-8");
    const { pools } = parsePools(poolsContent);
    const poolList = [...pools.values()].slice(0, 1000);

    // Build overrides for all tokens in the graph
    const allTokens = new Set<string>();
    for (const pool of poolList) {
        for (const t of pool.tokens) allTokens.add(t);
    }
    const tokenAmounts = new Map<string, bigint>();
    for (const t of allTokens) {
        tokenAmounts.set(t, 10n ** 36n);
    }
    const overrides = buildStateOverrides({
        routerAddress: ROUTER,
        tokenAmounts,
    });

    const harness = new Harness();
    await harness.waitReady();

    // Warm up
    await harness.findRoute(WAVAX, TOKENS[0].address, 10n ** 18n, poolsContent, 1000, overrides);

    const ratesBySize: Record<SizeKey, number[]> = { small: [], med: [], big: [] };
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
                const fwd = await harness.findRoute(WAVAX, token.address, vol.wavax, poolsContent, 1000, overrides);
                const fwdResult = fwd.result;
                if (!fwdResult?.steps || fwdResult.steps.length === 0) {
                    console.log(`  ${tag}: no forward route`);
                    totalMs += Date.now() - t0;
                    totalRoundtrips++;
                    continue;
                }

                const fwdOut = BigInt(fwdResult.amountOut);
                const rev = await harness.findRoute(token.address, WAVAX, fwdOut, poolsContent, 1000, overrides);
                const revResult = rev.result;
                if (!revResult?.steps || revResult.steps.length === 0) {
                    console.log(`  ${tag}: no reverse route`);
                    totalMs += Date.now() - t0;
                    totalRoundtrips++;
                    continue;
                }

                const elapsed = Date.now() - t0;
                totalMs += elapsed;
                totalRoundtrips++;

                const revOut = BigInt(revResult.amountOut);
                const rate = Number((revOut * 10000n) / vol.wavax) / 10000;
                ratesBySize[vol.key].push(rate);
                succBySize[vol.key]++;

                const pct = (rate * 100).toFixed(1);
                const hops = `${fwdResult.steps.length}→${revResult.steps.length}`;
                const fwdStats = fwdResult.stats;
                const revStats = revResult.stats;
                const totalQuotes = fwdStats.totalQuotes + revStats.totalQuotes;
                const formulaQuotes = fwdStats.formulaQuotes + revStats.formulaQuotes;
                const status = rate >= 0.9 ? "OK" : rate >= 0.5 ? "WEAK" : "BAD";
                console.log(
                    `  ${tag}: ${pct}% return (${hops} hops) ${elapsed}ms ${totalQuotes} quotes/${formulaQuotes} formula [${status}]`,
                );
            } catch (err: any) {
                totalMs += Date.now() - t0;
                totalRoundtrips++;
                console.log(`  ${tag}: ERROR — ${err.message?.slice(0, 80)}`);
            }
        }
    }

    harness.close();

    const result: Record<string, number> = {};
    for (const vol of VOLUMES) {
        const cov = Math.round((succBySize[vol.key] / totalBySize[vol.key]) * 1000) / 10;
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
