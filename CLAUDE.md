# DeFi Toolbox — Avalanche C-Chain

## Changelog (MANDATORY — enforced by pre-commit hook)

**Every commit MUST include changes to CHANGELOG.md.** A git pre-commit hook will reject commits without it — do not bypass it.

Add entries at the top in the existing format (date header + subsections with bullet points). Include:
- What changed and why
- Before/after results where applicable (metrics, error counts, performance)
- Dead-ends and investigations that didn't pan out (so they aren't repeated)

## Coverage Investigation

When investigating formula coverage gaps (blacklisted pools, EVM fallbacks), read [benchmarks/formula-accuracy/COVERAGE.md](benchmarks/formula-accuracy/COVERAGE.md) first. It contains:
- Current coverage state and breakdown
- Root causes already identified
- Investigation tools and techniques
- Key files and formula ID reference
- Priority fix order

**Agents working on coverage MUST update COVERAGE.md** with any new findings, tools, or techniques they discover.

## Project Structure

- `cmd/` — long-running services and production entrypoints (state-server, arb bots, quoter)
- `tools/` — run-once utilities that do a job and exit (discover, pool-collector)
- `benchmarks/` — performance and accuracy benchmarks
- `experiments/` — archived or in-progress prototypes

## Go Build Rules

**Never use `go build`** unless you need a binary for external use (e.g. WASM for JS integration). Only two commands:
- `go vet ./path/` — check compilation without producing artifacts
- `go run ./path/` — run (includes building), no leftover binaries
