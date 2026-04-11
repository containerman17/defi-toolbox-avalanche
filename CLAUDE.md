# DeFi Toolbox — Avalanche C-Chain

## Git Hooks (`.githooks/`)

Hooks are in `.githooks/` (committed). Activate with: `git config core.hooksPath .githooks`

Pre-commit hook enforces:
1. **Changelog** — every commit must include CHANGELOG.md changes.
2. **No overquoting** — reads `benchmarks/formula-accuracy/precision.txt`. Blocks commit if any formula returns more than EVM. Fix the formula or set the pool to -1 in `registry.txt`, then re-run the benchmark.

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

## Refactoring Policy

Full refactors and API changes are welcome — prioritize keeping the codebase clean over backwards compatibility. There are no external dependents; any downstream consumers will update when they pull. Spend the extra time to do it right rather than adding workarounds or parallel code paths.

## Go Build Rules

**Never use `go build`** unless you need a binary for external use (e.g. WASM for JS integration). Only two commands:
- `go vet ./path/` — check compilation without producing artifacts
- `go run ./path/` — run (includes building), no leftover binaries

## Verification Standard

Do not say a task is done unless it has been verified in the way the task actually matters.
`go vet` or a successful compile is not enough when the change affects runtime behavior.
If the work is supposed to function through a real script, pipeline, replay, or live startup
path, run that path and confirm the practical result before closing it out.

## Useful API Spells

Biglabs Avalanche arbitrages, filtered by sender and showing only block number + tx hash:

```bash
curl -s 'https://gateway.biglabs.eu/api/avalanche/arbitrages?per_page=2000&sortBy=-created_at' \
| jq -r '
  .[]
  | select((.sender | ascii_downcase) == "0x977a8afb38d7dfdc4aa438e883ca899d56dfabaa")
  | "\(.blockNumber)\t\(.hash)"
'
```
