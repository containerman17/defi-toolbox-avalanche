# Route Analyzer

Regression benchmark for HayabusaRouter. Replays real aggregator swaps (Odos, LFJ) against our router and compares output.

## Usage

Requires a local Avalanche node with `debug_traceCall` support.

```
export RPC_URL=http://localhost:9650/ext/bc/C/rpc
node convert.ts
node test.ts
```

## Files

- `txs.txt` — list of aggregator tx hashes to test
- `convert.ts` — replays each tx at block-1 via `debug_traceCall`, reconstructs the swap route from Transfer/Swap logs, writes payloads to `payloads/`
- `test.ts` — quotes each payload through HayabusaRouter at block-1, compares output vs aggregator's replay output
- `find_txs.ts` — helper to discover more Odos/LFJ swap txs in a block range
- `index.ts` — interactive single-tx analyzer

## Pass/Fail Logic

- PASS: our output >= aggregator's output
- FAIL: our output < aggregator's, or reverted, or 2x+ better (suspicious expected amount)
- Only valid reason to skip a tx during convert is RFQ (off-chain fill, not simulatable)

## Adding Transactions

Run `find_txs.ts` to discover new txs, append to `txs.txt`, then re-run convert + test.
