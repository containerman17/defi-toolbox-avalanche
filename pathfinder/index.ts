// Pathfinder — BFS-based multi-hop route discovery with layer-by-layer quoting
//
// Uses evm-quoter SDK (local EVM execution via native Go binary) instead of
// eth_call to RPC. State is cached by the state server.
//
// Algorithm:
//   1. buildGraph(pools) — adjacency table: token → [{pool, tokenOut}]
//   2. findBestRoute(quoter, graph, tokenIn, tokenOut, amountIn, maxHops):
//      - Layer 1: batch-quote all single-hop edges from tokenIn. Prune to best pool per token.
//      - Layer 2+: for each surviving token, batch-quote its edges. Prune again.
//      - Any time we reach tokenOut, record as terminal candidate.
//      - Return the best terminal.

import { type StoredPool } from "../pool-collector/index.ts";
import { decodeSwapResult } from "../evm-quoter/sdk.mjs";

export interface Edge {
  pool: StoredPool;
  tokenOut: string;
}

export interface TokenGraph {
  edges: Map<string, Edge[]>;
}

export interface RouteStep {
  pool: StoredPool;
  tokenIn: string;
  tokenOut: string;
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

/** A surviving candidate at an intermediate token after pruning. */
interface LayerNode {
  steps: RouteStep[];
  token: string;
  amount: bigint;
  visited: Set<string>;
}

/**
 * Find the best route from tokenIn to tokenOut by BFS with layer-by-layer quoting.
 *
 * Uses the evm-quoter SDK's quotePoolBatch for each layer — local EVM execution,
 * state cached by the state server.
 */
export async function findBestRoute(
  quoter: Quoter,
  graph: TokenGraph,
  tokenIn: string,
  tokenOut: string,
  amountIn: bigint,
  stateOverrides: any,
  maxHops: number = 4,
): Promise<{ route: RouteStep[]; amountOut: bigint; stats: { evmCalls: number; cacheMisses: number } } | null> {
  tokenIn = tokenIn.toLowerCase();
  tokenOut = tokenOut.toLowerCase();

  if (tokenIn === tokenOut) return null;

  let totalEvmCalls = 0;
  let totalCacheMisses = 0;

  let nodes: LayerNode[] = [
    { steps: [], token: tokenIn, amount: amountIn, visited: new Set([tokenIn]) },
  ];

  let bestTerminal: { route: RouteStep[]; amountOut: bigint } | null = null;

  for (let layer = 0; layer < maxHops; layer++) {
    interface HopQuote {
      nodeIdx: number;
      step: RouteStep;
      targetToken: string;
      amountIn: bigint;
    }

    const hops: HopQuote[] = [];

    for (let ni = 0; ni < nodes.length; ni++) {
      const node = nodes[ni];
      const neighbors = graph.edges.get(node.token);
      if (!neighbors) continue;

      const seen = new Set<string>();

      for (const edge of neighbors) {
        if (node.visited.has(edge.tokenOut) && edge.tokenOut !== tokenOut) continue;

        const key = `${edge.pool.address}:${edge.tokenOut}`;
        if (seen.has(key)) continue;
        seen.add(key);

        hops.push({
          nodeIdx: ni,
          step: { pool: edge.pool, tokenIn: node.token, tokenOut: edge.tokenOut },
          targetToken: edge.tokenOut,
          amountIn: node.amount,
        });
      }
    }

    if (hops.length === 0) break;

    // Batch-quote all hops for this layer via local EVM
    const batchInput = hops.map(hop => ({
      pool: hop.step.pool.address,
      poolType: hop.step.pool.poolType,
      tokenIn: hop.step.tokenIn,
      tokenOut: hop.step.tokenOut,
      amountIn: hop.amountIn,
    }));

    const batchResults = await quoter.quotePoolBatch(batchInput, stateOverrides) as any;
    totalEvmCalls += hops.length;
    totalCacheMisses += batchResults.cacheMisses || 0;

    // Process results
    const bestPerToken = new Map<string, LayerNode>();

    for (let i = 0; i < hops.length; i++) {
      const r = batchResults[i];
      if (!r.ok || !r.returnData) continue;

      let out: bigint;
      try {
        out = decodeSwapResult(r.returnData as `0x${string}`);
      } catch {
        continue;
      }
      if (out === 0n) continue;

      const hop = hops[i];
      const parentNode = nodes[hop.nodeIdx];
      const fullRoute = [...parentNode.steps, hop.step];

      if (hop.targetToken === tokenOut) {
        if (!bestTerminal || out > bestTerminal.amountOut) {
          bestTerminal = { route: fullRoute, amountOut: out };
        }
        continue;
      }

      const existing = bestPerToken.get(hop.targetToken);
      if (!existing || out > existing.amount) {
        const newVisited = new Set(parentNode.visited);
        newVisited.add(hop.targetToken);
        bestPerToken.set(hop.targetToken, {
          steps: fullRoute,
          token: hop.targetToken,
          amount: out,
          visited: newVisited,
        });
      }
    }

    nodes = [...bestPerToken.values()];
    if (nodes.length === 0) break;
  }

  if (!bestTerminal) return null;
  return { ...bestTerminal, stats: { evmCalls: totalEvmCalls, cacheMisses: totalCacheMisses } };
}
