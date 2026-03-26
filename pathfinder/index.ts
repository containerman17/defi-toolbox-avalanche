// Pathfinder — simple 2-hop route finder with batch quoting
//
// Algorithm:
//   Phase 1: Quote all direct pools tokenIn → tokenOut, keep best.
//   Phase 2: Find intermediate tokens adjacent to both tokenIn and tokenOut.
//            Quote hop 1 (keep best per intermediate), then hop 2.
//
// Max 2 hops, no path splitting.

import { type StoredPool } from "../pool-collector/types.ts";
import { decodeSwapResult, type RouteStep } from "../router/encode.ts";

export type { RouteStep };

export interface Edge {
  pool: StoredPool;
  tokenOut: string;
}

export interface TokenGraph {
  edges: Map<string, Edge[]>;
}

/** Quoter interface — subset of what createQuoter() returns. */
export interface Quoter {
  quotePoolBatch(pools: { pool: string; poolType: number; tokenIn: string; tokenOut: string; amountIn: bigint }[], stateOverrides?: any): Promise<{ ok: boolean; returnData?: string; error?: string }[]>;
  close(): void;
}

/**
 * Build a token adjacency graph from pools.
 * Each pool with tokens [A, B] creates edges A→B and B→A.
 * Multi-token pools create edges for every ordered pair.
 */
export function buildGraph(pools: Iterable<StoredPool>): TokenGraph {
  const edges = new Map<string, Edge[]>();

  function addEdge(tokenIn: string, tokenOut: string, pool: StoredPool) {
    let list = edges.get(tokenIn);
    if (!list) {
      list = [];
      edges.set(tokenIn, list);
    }
    list.push({ pool, tokenOut });
  }

  for (const pool of pools) {
    const tokens = pool.tokens;
    for (let i = 0; i < tokens.length; i++) {
      for (let j = 0; j < tokens.length; j++) {
        if (i !== j) {
          addEdge(tokens[i], tokens[j], pool);
        }
      }
    }
  }

  return { edges };
}

/**
 * Find the best route from tokenIn to tokenOut (max 2 hops).
 *
 * Phase 1: batch-quote all direct pools.
 * Phase 2: find intermediates, batch-quote hop 1, keep best per intermediate,
 *          batch-quote hop 2.
 */
export async function findBestRoute(
  quoter: Quoter,
  graph: TokenGraph,
  tokenIn: string,
  tokenOut: string,
  amountIn: bigint,
  stateOverrides: any,
): Promise<{ route: RouteStep[]; amountOut: bigint; stats: { totalQuotes: number } } | null> {
  tokenIn = tokenIn.toLowerCase();
  tokenOut = tokenOut.toLowerCase();

  if (tokenIn === tokenOut) return null;

  let totalQuotes = 0;
  let bestRoute: RouteStep[] | null = null;
  let bestAmountOut = 0n;

  const edgesFromIn = graph.edges.get(tokenIn) || [];

  // ── Phase 1: Direct pools (1 hop) ──────────────────────────────────
  const directHops: { edge: Edge; idx: number }[] = [];
  const directBatch: { pool: string; poolType: number; tokenIn: string; tokenOut: string; amountIn: bigint }[] = [];

  for (const edge of edgesFromIn) {
    if (edge.tokenOut !== tokenOut) continue;
    directHops.push({ edge, idx: directBatch.length });
    directBatch.push({
      pool: edge.pool.address,
      poolType: edge.pool.poolType,
      tokenIn,
      tokenOut,
      amountIn,
    });
  }

  if (directBatch.length > 0) {
    const results = await quoter.quotePoolBatch(directBatch, stateOverrides);
    totalQuotes += directBatch.length;

    for (const { edge, idx } of directHops) {
      const r = results[idx];
      if (!r.ok || !r.returnData) continue;
      let out: bigint;
      try { out = decodeSwapResult(r.returnData as `0x${string}`); } catch { continue; }
      if (out > bestAmountOut) {
        bestAmountOut = out;
        bestRoute = [{ pool: edge.pool, tokenIn, tokenOut }];
      }
    }
  }

  // ── Phase 2: Two-hop via intermediate tokens ───────────────────────

  // Collect hop-1 candidates: edges from tokenIn to intermediates that also connect to tokenOut
  const tokenOutNeighbors = new Set<string>();
  for (const edge of graph.edges.get(tokenOut) || []) {
    tokenOutNeighbors.add(edge.tokenOut); // reverse: if X→tokenOut exists, tokenOut→X also exists in undirected graph
  }
  // Actually: graph edges are directed per-pair. edges.get(tokenOut) gives edges FROM tokenOut.
  // But BuildGraph creates both directions, so edges FROM tokenOut include all tokens adjacent to tokenOut.
  // So tokenOutNeighbors = set of tokens that have a pool connecting them to tokenOut. ✓
  // We also need: edges from intermediate TO tokenOut. Since graph is bidirectional, if mid is in
  // tokenOutNeighbors, then edges.get(mid) will have an edge with tokenOut == tokenOut.

  // But more directly: for each intermediate, check if edges.get(mid) has tokenOut
  // We pre-compute the set for O(1) lookup
  const midConnectsToOut = new Set<string>();
  // We could scan all edges, but cheaper: just check tokenOutNeighbors
  // If tokenOut→X edge exists, then X→tokenOut edge also exists (bidirectional graph)
  for (const t of tokenOutNeighbors) {
    midConnectsToOut.add(t);
  }

  type Hop1Candidate = { edge: Edge; batchIdx: number };
  const hop1Candidates: Hop1Candidate[] = [];
  const hop1Batch: { pool: string; poolType: number; tokenIn: string; tokenOut: string; amountIn: bigint }[] = [];
  const seenKeys = new Set<string>();

  for (const edge of edgesFromIn) {
    const mid = edge.tokenOut;
    if (mid === tokenIn || mid === tokenOut) continue;
    if (!midConnectsToOut.has(mid)) continue;

    const key = `${edge.pool.address}:${mid}`;
    if (seenKeys.has(key)) continue;
    seenKeys.add(key);

    hop1Candidates.push({ edge, batchIdx: hop1Batch.length });
    hop1Batch.push({
      pool: edge.pool.address,
      poolType: edge.pool.poolType,
      tokenIn,
      tokenOut: mid,
      amountIn,
    });
  }

  if (hop1Batch.length === 0) {
    return bestRoute ? { route: bestRoute, amountOut: bestAmountOut, stats: { totalQuotes } } : null;
  }

  // Batch-quote hop 1
  const hop1Results = await quoter.quotePoolBatch(hop1Batch, stateOverrides);
  totalQuotes += hop1Batch.length;

  // Keep best per intermediate
  const bestPerMid = new Map<string, { step: RouteStep; amount: bigint }>();
  for (const { edge, batchIdx } of hop1Candidates) {
    const r = hop1Results[batchIdx];
    if (!r.ok || !r.returnData) continue;
    let out: bigint;
    try { out = decodeSwapResult(r.returnData as `0x${string}`); } catch { continue; }
    if (out === 0n) continue;

    const mid = edge.tokenOut;
    const existing = bestPerMid.get(mid);
    if (!existing || out > existing.amount) {
      bestPerMid.set(mid, {
        step: { pool: edge.pool, tokenIn, tokenOut: mid },
        amount: out,
      });
    }
  }

  if (bestPerMid.size === 0) {
    return bestRoute ? { route: bestRoute, amountOut: bestAmountOut, stats: { totalQuotes } } : null;
  }

  // Build hop 2 batch: for each surviving intermediate, all pools to tokenOut
  type Hop2Candidate = { mid: string; edge: Edge; batchIdx: number };
  const hop2Candidates: Hop2Candidate[] = [];
  const hop2Batch: { pool: string; poolType: number; tokenIn: string; tokenOut: string; amountIn: bigint }[] = [];

  for (const [mid, hop1] of bestPerMid) {
    const seenPools = new Set<string>();
    for (const edge of graph.edges.get(mid) || []) {
      if (edge.tokenOut !== tokenOut) continue;
      if (seenPools.has(edge.pool.address)) continue;
      seenPools.add(edge.pool.address);

      hop2Candidates.push({ mid, edge, batchIdx: hop2Batch.length });
      hop2Batch.push({
        pool: edge.pool.address,
        poolType: edge.pool.poolType,
        tokenIn: mid,
        tokenOut,
        amountIn: hop1.amount,
      });
    }
  }

  if (hop2Batch.length > 0) {
    const hop2Results = await quoter.quotePoolBatch(hop2Batch, stateOverrides);
    totalQuotes += hop2Batch.length;

    for (const { mid, edge, batchIdx } of hop2Candidates) {
      const r = hop2Results[batchIdx];
      if (!r.ok || !r.returnData) continue;
      let out: bigint;
      try { out = decodeSwapResult(r.returnData as `0x${string}`); } catch { continue; }
      if (out > bestAmountOut) {
        bestAmountOut = out;
        const hop1 = bestPerMid.get(mid)!;
        bestRoute = [
          hop1.step,
          { pool: edge.pool, tokenIn: mid, tokenOut },
        ];
      }
    }
  }

  if (!bestRoute) return null;
  return { route: bestRoute, amountOut: bestAmountOut, stats: { totalQuotes } };
}
