// bench.mjs — Single entry point for all benchmarks
//
// Usage:
//   node bench.mjs pathfinder/roundtrip          # run one, append to results file
//   node bench.mjs pathfinder/roundtrip --dry-run # run one, don't write
//   node bench.mjs pathfinder/roundtrip 1         # run with count=1
//   node bench.mjs                                # run all, append to results files
//
// Results are stored in benchmark_results/<name>.log (slashes → underscores)
// Each line: time=2026-03-24_02:51 git=abc1234 key1=val1 key2=val2 ...

import { appendFileSync, mkdirSync } from "node:fs";
import { execSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const RESULTS_DIR = join(__dirname, "benchmark_results");

const benchmarks = {
  "evm/correctness":    { module: "./evm-quoter/benchmarks/01_correctness.ts", defaultCount: 4000 },
  "evm/speed":          { module: "./evm-quoter/benchmarks/02_speed.ts",       defaultCount: 4000 },
  "evm/coverage":       { module: "./evm-quoter/benchmarks/03_coverage.ts",     defaultCount: 5000 },
  "router/backrun_lfj": { module: "./router/benchmarks/backrun_lfj/03_test.ts",  defaultCount: 2000 },
  "pathfinder/roundtrip": { module: "./pathfinder/benchmarks/bench.ts",           defaultCount: 3 },
};

async function runOne(name, count) {
  const bench = benchmarks[name];
  if (!bench) {
    console.error(`Unknown benchmark: ${name}`);
    console.error(`Available: ${Object.keys(benchmarks).join(", ")}`);
    process.exit(1);
  }

  const effectiveCount = count || bench.defaultCount;
  console.log(`\n=== ${name} (${effectiveCount}) ===\n`);
  const { run } = await import(bench.module);
  const result = await run(effectiveCount);
  return { name, result };
}

function formatResult(name, result) {
  // Flatten result to key=value pairs
  const pairs = [];
  if (typeof result === "number") {
    pairs.push(`result=${result}`);
  } else {
    for (const [k, v] of Object.entries(result)) {
      pairs.push(`${k}=${v}`);
    }
  }
  return pairs;
}

function appendResult(name, result) {
  mkdirSync(RESULTS_DIR, { recursive: true });
  const fileName = name.replace(/\//g, "_") + ".log";
  const filePath = join(RESULTS_DIR, fileName);

  const date = new Date().toISOString().slice(0, 16).replace("T", "_");
  let gitHash;
  try { gitHash = execSync("git rev-parse --short HEAD").toString().trim(); }
  catch { gitHash = "unknown"; }

  const pairs = formatResult(name, result);
  const line = `time=${date} git=${gitHash} ${pairs.join(" ")}\n`;

  appendFileSync(filePath, line);
  console.log(`\nAppended to ${filePath}`);
}

// Parse args
const args = process.argv.slice(2);
const dryRun = args.includes("--dry-run");
const positional = args.filter(a => a !== "--dry-run");
const benchName = positional.find(a => !/^\d+$/.test(a));
const count = parseInt(positional.find(a => /^\d+$/.test(a)) || "0", 10) || undefined;

if (benchName) {
  const { name, result } = await runOne(benchName, count);
  const pairs = formatResult(name, result);
  console.log(`\n>>> ${pairs.join(" ")}`);
  if (!dryRun) appendResult(name, result);
  process.exit(0);
}

// Run all benchmarks
for (const name of Object.keys(benchmarks)) {
  const { result } = await runOne(name, count);
  const pairs = formatResult(name, result);
  console.log(`\n>>> ${name}: ${pairs.join(" ")}`);
  if (!dryRun) appendResult(name, result);
}
