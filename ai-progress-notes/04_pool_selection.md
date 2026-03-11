# Pool Selection (Prescreen)

## Why

BFS routing is powerful but expensive — it quotes every pool edge at every layer via EVM execution. With 7,000+ discovered pools, running BFS on the full set takes too long for real-time use. On production hardware, a 500-pool BFS takes ~85-100ms; the full pool set would take seconds.

Pool selection (prescreening) narrows the universe to the ~500-1,000 pools that actually matter for a given (token_in, token_out, amount) query. The rest are either illiquid, unreachable, or dominated by better alternatives.

## How It Works: Three Phases

### Phase 0 — Token Pricing

Price all reachable tokens relative to WAVAX using a 2-wave BFS:

- **Wave 1**: Quote all direct WAVAX neighbors with a small probe (~0.1 AVAX). This prices the major tokens (USDC, USDT, etc.).
- **Wave 2**: For each newly priced token, quote its unpriced neighbors. This captures tokens that are only reachable via an intermediate hop.

Two waves is enough — tokens requiring 3+ hops to reach WAVAX are too obscure to matter. Result: ~3,800 tokens priced in ~120ms (production hardware: ~15ms).

### Phase 1 — Pool Rate Table

For every pool edge, probe at the exact size tiers that TypeScript requested. If TypeScript sent sizes `[0.01, 0.1, 1, 10, 100]` AVAX, Phase 1 quotes each edge at those 5 amounts. No interpolation — Phase 2 gets exact rates for the sizes it cares about.

Amounts are price-scaled using Phase 0 prices: converted from AVAX-equivalent to the edge's input token. If a token costs 0.001 AVAX, the "1 AVAX" tier sends 1,000 tokens.

Result: a `(size_tier, rate)` table for every edge. ~22,000 edges × N tiers EVM calls, ~600ms (production: ~60ms for 5 tiers).

### Phase 2 — f64 Path Enumeration

Fast float-based path enumeration using the rate table from Phase 1. No EVM calls — purely arithmetic. Runs once per size tier (since rates differ per tier).

**1-3 hops (full enumeration):**
```
for e1 in edges(token_in):
    amt1 = input_amount * e1.rate[tier]
    if e1 reaches token_out → score(e1)

    for e2 in edges(e1.token_out):
        amt2 = amt1 * e2.rate[tier]
        if e2 reaches token_out → score(e1, e2)

        for e3 in edges(e2.token_out):
            amt3 = amt2 * e3.rate[tier]
            if e3 reaches token_out → score(e1, e2, e3)
```

**4-hop (meet-in-the-middle):** To avoid O(N^4) explosion, collect all forward 2-hop arrivals at intermediate tokens during the 1-3 hop loop. Separately enumerate 2-hop paths backward from token_out. Combine forward and backward halves through matching intermediate tokens.

**Scoring:** Each pool accumulates the best `amount_out` of any complete path it participates in. Top ~1,000 pools by score are selected, plus ~50 guaranteed from each side (direct token_in and token_out edges) to avoid missing obvious routes.

Phase 2 is pure f64 math — sub-millisecond even for large graphs.

## Architecture: TypeScript Controls, Rust Executes

### Design Principle

Rust ships as a compiled binary inside npm — hard to change. TypeScript is the user-facing scripting layer — easy to iterate. Therefore: **Rust is a powerful stateful executor; TypeScript is the brain that orchestrates.**

### Operations

| Operation | Payload | What Rust does |
|-----------|---------|----------------|
| `set_pools` | Pool list from discovery | Store pool universe, assign indices |
| `prepare` | `directions: [{token_in, token_out, size, max_hops}, …]` | Phase 0+1 (unique sizes) → Phase 2 (per direction). Store narrowed pool sets |
| `narrow` | `directions: [{token_in, token_out, size, max_hops}, …]` | Phase 2 only, reuses cached rates. For adding directions without rebuilding |
| `bfs` | `token_in, token_out, amount` | Phase 3 on stored narrowed set. Snaps to nearest direction's size; warns if >10× off |

**Typical flow:**

```
TypeScript                              Rust
    │                                     │
    │  set_pools(pools)                   │
    ├────────────────────────────────────►│  Store universe
    │                                     │
    │  prepare([                          │
    │    {USDC, WAVAX, 1 AVAX, 4},        │
    │    {WAVAX, USDC, 1 AVAX, 2},        │
    │    {USDC, WAVAX, 100 AVAX, 3},      │
    │    …10 more directions…             │
    │  ])                                 │
    ├────────────────────────────────────►│  Phase 0: price tokens (once)
    │                                     │  Phase 1: rate table at unique
    │                                     │    sizes {1, 100} AVAX (once)
    │                                     │  Phase 2: narrow per direction
    │                                     │    (~30-70ms each, sequential)
    │         { ok, narrowed: [847, …] }  │  Store narrowed sets
    │◄────────────────────────────────────┤
    │                                     │
    │  bfs(USDC, WAVAX, 1.5 AVAX)        │
    ├────────────────────────────────────►│  Snap to (USDC,WAVAX,1 AVAX) set
    │                                     │  Phase 3 on narrowed pools
    │         { route, amount_out, gas }  │
    │◄────────────────────────────────────┤
```

- **`prepare` is one round-trip** for all directions. Rust extracts unique sizes, runs Phase 0+1 once, then Phase 2 per direction. No latency gap between rate building and narrowing.
- **Phase 2 is not sub-millisecond.** The nested enumeration over 7k×7k edges takes 30-70ms per direction on high-end CPUs and isn't easily parallelizable.
- **Pool lists stay inside Rust.** TypeScript gets back pool counts (optionally indices for debug via `verbose` flag).
- **`narrow` reuses cached rates** for adding directions later without the Phase 0+1 cost. Only valid until the next `prepare` rebuilds rates.
- **`max_hops` is per direction** because narrowing for 2 hops and 4 hops produces very different pool sets. A 2-hop narrowing only explores direct neighbors of token_in and token_out — it completely misses intermediate pools that a 4-hop path would traverse. Narrowing for 4 hops naturally includes all good 2-hop pools (they'd score even higher with fewer hops), but not vice versa. The BFS `max_hops` should match what was prepared.
- **BFS snaps to nearest prepared direction.** Rust converts the BFS amount to AVAX-equivalent using Phase 0 prices and picks the narrowed set from the closest matching (token_in, token_out, size). Warns if >10× off.

### Refresh Cadence

On production hardware, Phase 0+1 takes ~140ms. Phase 2 takes ~30-70ms per direction. For 10 directions at 2 sizes each, total `prepare` is ~140ms + 20 × 50ms ≈ 1.1s. Feasible every few blocks; TypeScript decides when.

State diffs are applied every block regardless (via `apply_diff` or follow mode), so BFS always quotes against fresh state even if pool narrowing is a few blocks stale.

### Why Not Return the Rate Table to TypeScript?

Phase 1 produces ~1.7MB of rate data for 7,000 pools. Serializing and parsing this every block adds 2-5ms+ and bloats the IPC channel. Phase 2 (the consumer) does millions of float multiply-accumulates across adjacency lists — heavy work that stays in Rust. Only the narrowed pool counts cross the boundary.

## Future Considerations

- **Incremental Phase 1**: When only a few pools' state changed (small block diff), re-probe only affected edges instead of the full rate table.
- **Per-direction BFS hinting**: `bfs` could accept an explicit direction key so Rust doesn't need to snap/guess.
