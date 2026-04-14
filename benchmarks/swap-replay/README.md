# Swap Replay

Go swap-replay benchmark.

Each run fetches LFJ swap logs directly from RPC, starting at the Hayabusa
router deployment block, then simulates the original transaction payload
(`from`, `to`, `data`, `value`) at `block - 1` on fixed-block lightclient
state. It then traces that same `block - 1` simulation, reconstructs a
single-path route from the trace transfers when possible, and replays the route
through HayabusaRouter on the same fixed-block state. Finally, it runs the
no-split BFS pathfinder with only `tokenIn`, `tokenOut`, and `amountIn`,
replays the found route through HayabusaRouter on the same fixed-block state,
and compares that executed output against the traced oracle.

There are no tx list inputs and no mode flags in the live benchmark.

Usage:

```bash
go run ./benchmarks/swap-replay/
go run ./benchmarks/swap-replay/ --limit 25
go run ./benchmarks/swap-replay/ --limit 25 --start-block 82700000 --end-block 82750000
go run ./benchmarks/swap-replay/ --rpc http://localhost:9650/ext/bc/C/rpc --ws ws://localhost:9650/ext/bc/C/ws
```

Flags:

- `--limit`: number of transactions to print, default `10`, `0 = all`
- `--rpc`: HTTP RPC URL, default `http://localhost:9650/ext/bc/C/rpc`
- `--ws`: WebSocket RPC URL for fixed-block lightclient replay
- `--data-dir`: snapshot/cache directory for fixed-block lightclient replay
- `--start-block`: first block to scan, default Hayabusa deployment block `82067033`
- `--end-block`: last block to scan, default latest block
- `--chunk-size`: block range per `eth_getLogs` request, default `50000`

Legacy route-reconstruction benchmark code now lives in
`archive/benchmarks/swap-replay-legacy/`.
