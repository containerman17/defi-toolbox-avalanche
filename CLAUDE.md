# DeFi Toolbox — Avalanche C-Chain

## Changelog

Keep [CHANGELOG.md](CHANGELOG.md) updated after every significant change, investigation, or dead-end. Add entries at the top in the existing format (date header + subsections with bullet points).

## Coverage Investigation

When investigating formula coverage gaps (blacklisted pools, EVM fallbacks), read [COVERAGE.md](COVERAGE.md) first. It contains:
- Current coverage state and breakdown
- Root causes already identified
- Investigation tools and techniques
- Key files and formula ID reference
- Priority fix order

**Agents working on coverage MUST update COVERAGE.md** with any new findings, tools, or techniques they discover.

## Go Build Rules

**Never use `go build`** unless you need a binary for external use (e.g. WASM for JS integration). Only two commands:
- `go vet ./path/` — check compilation without producing artifacts
- `go run ./path/` — run (includes building), no leftover binaries
