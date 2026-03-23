// bench.mjs — Single entry point for all benchmarks
//
// Usage:
//   node bench.mjs                              # run all, write to results file
//   node bench.mjs --dry-run                    # run all, don't write
//   node bench.mjs evm/correctness 4000         # run one benchmark
//   node bench.mjs evm/speed 1000
//   node bench.mjs evm/coverage 5000
//   node bench.mjs router/backrun_lfj 2000

import { appendFileSync } from "node:fs";
import { execSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const RESULTS_FILE = join(__dirname, "benchmark_results.yaml");

const benchmarks = {
  "evm/correctness":    { module: "./evm-quoter/benchmarks/01_correctness.mjs", defaultCount: 4000, unit: "mismatches" },
  "evm/speed":          { module: "./evm-quoter/benchmarks/02_speed.mjs",       defaultCount: 4000, unit: "ms/pool" },
  "evm/coverage":       { module: "./evm-quoter/benchmarks/03_coverage.mjs",     defaultCount: 5000, unit: "%" },
  "router/backrun_lfj": { module: "./router/benchmarks/backrun_lfj/03_test.ts",  defaultCount: 2000, unit: "% pass" },
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
  return { name, count: effectiveCount, result, unit: bench.unit };
}

// Parse args
const args = process.argv.slice(2);
const dryRun = args.includes("--dry-run");
const positional = args.filter(a => a !== "--dry-run");
const benchName = positional.find(a => !/^\d+$/.test(a));
const count = parseInt(positional.find(a => /^\d+$/.test(a)) || "0", 10) || undefined;

if (benchName) {
  // Run single benchmark (never writes)
  const { result, unit } = await runOne(benchName, count);
  console.log(`\n>>> ${benchName}: ${result} ${unit}`);
  process.exit(0);
}

// Run all benchmarks
const results = {};
for (const name of Object.keys(benchmarks)) {
  const { result } = await runOne(name, count);
  results[name] = result;
}

console.log("\n=== Summary ===\n");
for (const [name, bench] of Object.entries(benchmarks)) {
  console.log(`  ${name}: ${results[name]} ${bench.unit}`);
}

if (dryRun) {
  console.log("\n(dry run, not writing)");
  process.exit(0);
}

// Append YAML entry
const date = new Date().toISOString().slice(0, 19).replace("T", " ");
let gitHash;
try { gitHash = execSync("git rev-parse --short HEAD").toString().trim(); }
catch { gitHash = "unknown"; }

const entry = [
  `- date: "${date}"`,
  `  git: ${gitHash}`,
  ...Object.entries(benchmarks).map(([name, bench]) =>
    `  ${name}: ${results[name]}  # ${bench.unit}`
  ),
  "",
].join("\n");

appendFileSync(RESULTS_FILE, entry);
console.log(`\nAppended to ${RESULTS_FILE}`);
