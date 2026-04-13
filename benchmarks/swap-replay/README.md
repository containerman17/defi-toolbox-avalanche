# Swap Replay

Minimal Go swap-replay benchmark.

Each run fetches LFJ swap logs directly from RPC, starting at the LFJ router
deployment block, and prints a compact per-transaction summary with token names
and decimal-formatted amounts.

There are no tx list inputs or generated tx cache files in the live benchmark.

Usage:

```bash
go run ./benchmarks/swap-replay/
go run ./benchmarks/swap-replay/ --limit 25
go run ./benchmarks/swap-replay/ --limit 25 --start-block 82700000 --end-block 82750000
go run ./benchmarks/swap-replay/ --rpc http://localhost:9650/ext/bc/C/rpc
```

Flags:

- `--limit`: number of transactions to print, default `10`, `0 = all`
- `--rpc`: HTTP RPC URL, default `http://localhost:9650/ext/bc/C/rpc`
- `--start-block`: first block to scan, default LFJ deployment block `80091636`
- `--end-block`: last block to scan, default latest block
- `--chunk-size`: block range per `eth_getLogs` request, default `50000`

Legacy route-reconstruction benchmark code now lives in
`archive/benchmarks/swap-replay-legacy/`.
