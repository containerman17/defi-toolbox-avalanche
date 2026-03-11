// Reads tx hashes from txs.txt, replays each at block-1 (start of block)
// via debug_traceCall, extracts routes from Transfer logs, and writes
// HayabusaRouter payloads to payloads/<hash>.json.
//
// Replaying at block-1 ensures the expectedOut reflects block-start state,
// filtering out backruns (txs that revert without preceding state setup).
//
// Usage: node convert.ts [txs.txt]
// Requires local RPC with debug_traceCall support.

import * as fs from "node:fs";
import * as path from "node:path";
import { createPublicClient, http, decodeAbiParameters, encodeAbiParameters, type Hex, type Log } from "viem";
import { wsPool } from "../../rpc/ws-pool.ts";
import { avalanche } from "viem/chains";
import { loadPools, ERC4626_VAULTS, generateBufferedEdges, type StoredPool } from "hayabusa-pools";

// --- Constants ---

const TRANSFER_TOPIC = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef";
const V4_SWAP_TOPIC = "0x40e9cecb9f5f1f1c5b9c97dec2917b7ee92e57ba5563708daca94dd84ad7112f";
const WOMBAT_SWAP_TOPIC = "0x54787c404bb33c88e86f4baf88183a3b0141d0a848e6a9f7a13b66ae3a9b73d1";
const ZERO_ADDR = "0x0000000000000000000000000000000000000000";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const WOOFI_ROUTER = "0x4c4af8dbc524681930a27b2f1af5bcc8062e6fb7";
const WOOPP_ADDRESS = "0x5520385bfcf07ec87c4c53a7d8d65595dff69fa4";
const BALANCER_V2_VAULT = "0xba12222222228d8ba445958a75a0704d566bf2c8";
const WOOPP_V2_ADDRESS = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const BALANCER_V3_VAULT = "0xba1333333333a1ba1108e8412f11850a5c319ba9";
const CAVALRE_POOL = "0x5f1e8ed8468232bab71eda9f4598bda3161f48ea";
const BENTOBOX = "0x0711b6026068f736bae6b213031fce978d48e026";
// Synapse stableswap
const SYNAPSE_TOKEN_SWAP_TOPIC = "0xc6c1e0630dbe9130cc068028486c0d118ddcea348550819defd5cb8c257f8a38";
const SYNAPSE_TOKENS: Record<string, string[]> = {
  "0xed2a7edd7413021d440b09d654f3b87712abab66": [
    "0xcfc37a6ab183dd4aed08c204d1c2773c0b1bdf46", // 0: nUSD
    "0xd586e7f844cea2f87f50152665bcbc2c279d8d70", // 1: DAI.e
    "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664", // 2: USDC.e
    "0xc7198437980c041c805a1edcba50c1ce5db95118", // 3: USDt.e
  ],
  "0xa196a03653f6cc5ca0282a8bd7ec60e93f620afc": [
    "0xcfc37a6ab183dd4aed08c204d1c2773c0b1bdf46", // 0: nUSD
    "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e", // 1: USDC
    "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7", // 2: USDt
  ],
};

// Bridge-equivalent tokens: .e (bridged) versions have ~1:1 value with their
// native counterparts. Platypus/Wombat pools swap between these at near-parity.
const BRIDGE_EQUIVALENTS: Record<string, string> = {
  "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664": "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e", // USDC.e → USDC
  "0xc7198437980c041c805a1edcba50c1ce5db95118": "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7", // USDt.e → USDt
  "0x0000000000000000000000000000000000000000": "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7", // AVAX → WAVAX
};

function tokenMatchesOutput(token: string, outputToken: string): boolean {
  if (token === outputToken) return true;
  return BRIDGE_EQUIVALENTS[token] === outputToken;
}

// RFQ / off-chain pools — not simulatable via on-chain swap().
// IMPORTANT: This is a manually curated allowlist. Do NOT replace with heuristics.
// If a pool uses off-chain signed quotes (Hashflow, Dexalot, etc.) or is otherwise
// not reproducible via eth_call, add its address here. Keep the list explicit —
// even if it gets long, that's fine. We want exact control over what gets skipped.
const RFQ_POOLS = new Set([
  "0xeed3c159f3a96ab8d41c8b9ca49ee1e5071a7cdd", // Dexalot MainnetRFQ (impl 0x64AE65F4)
]);

// Hashflow RFQ vaults: EOAs that hold tokens and execute signed-quote swaps.
// Instead of skipping, we simulate via TRANSFER_FROM: override vault's balance+allowance
// and pull the output token directly. The amounts come from the Transfer events.
const HASHFLOW_VAULTS = new Set([
  "0x6047b384d58dc7f8f6fef85d75754e6928f06484", // Hashflow token vault (custody for pool 0x012cb12e)
]);
const HASHFLOW_POOLS = new Set([
  "0x012cb12e50e0467f4844dc0046d2604fce940ad7", // Hashflow RFQ pool (EIP-1167 proxy) — emits Trade events
]);

const KNOWN_TOKENS: Record<string, { symbol: string; decimals: number }> = {
  "0x0000000000000000000000000000000000000000": { symbol: "AVAX", decimals: 18 },
  "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7": { symbol: "WAVAX", decimals: 18 },
  "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e": { symbol: "USDC", decimals: 6 },
  "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7": { symbol: "USDt", decimals: 6 },
  "0x49d5c2bdffac6ce2bfdb6640f4f80f226bc10bab": { symbol: "WETH.e", decimals: 18 },
  "0x2b2c81e08f1af8835a78bb2a90ae924ace0ea4be": { symbol: "sAVAX", decimals: 18 },
  "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664": { symbol: "USDC.e", decimals: 6 },
  "0xc7198437980c041c805a1edcba50c1ce5db95118": { symbol: "USDt.e", decimals: 6 },
};

function tok(addr: string): string {
  return KNOWN_TOKENS[addr.toLowerCase()]?.symbol ?? addr.slice(0, 10);
}

// --- Router detection ---

interface RouterDef {
  name: string;
  address: string;
  swapTopic: string;
  parseSwapEvent(data: string): { inputToken: string; outputToken: string; inputAmount: bigint; outputAmount: bigint };
}

const ROUTERS: RouterDef[] = [
  {
    name: "odos",
    address: "0x0d05a7d3448512b78fa8a9e46c4872c88c4a0d05",
    swapTopic: "0x69db20ca9e32403e6c56e5193b3e3b2827ae5c430ccfdea392ba950d2d1ab2bc",
    parseSwapEvent(data: string) {
      return {
        inputAmount: BigInt("0x" + data.slice(66, 130)),
        inputToken: ("0x" + data.slice(154, 194)).toLowerCase(),
        outputAmount: BigInt("0x" + data.slice(194, 258)),
        outputToken: ("0x" + data.slice(282, 322)).toLowerCase(),
      };
    },
  },
  {
    name: "lfj",
    address: "0x45a62b090df48243f12a21897e7ed91863e2c86b",
    swapTopic: "0xd9a8cfa901e597f6bbb7ea94478cf9ad6f38d0dc3fd24d493e99cb40692e39f1",
    parseSwapEvent(data: string) {
      return {
        inputToken: ("0x" + data.slice(90, 130)).toLowerCase(),
        outputToken: ("0x" + data.slice(154, 194)).toLowerCase(),
        inputAmount: BigInt("0x" + data.slice(194, 258)),
        outputAmount: BigInt("0x" + data.slice(258, 322)),
      };
    },
  },
];

// --- Transfer parsing ---

interface TransferEvent {
  token: string;
  from: string;
  to: string;
  amount: bigint;
  logIndex: number;
}

function parseTransfers(logs: Log[]): TransferEvent[] {
  const transfers: TransferEvent[] = [];
  for (const log of logs) {
    if (log.topics[0] === TRANSFER_TOPIC && log.topics.length >= 3) {
      transfers.push({
        token: log.address.toLowerCase(),
        from: ("0x" + log.topics[1]!.slice(26)).toLowerCase(),
        to: ("0x" + log.topics[2]!.slice(26)).toLowerCase(),
        amount: log.data && log.data.length > 2 ? BigInt(log.data) : 0n,
        logIndex: log.logIndex ?? 0,
      });
    }
  }
  return transfers;
}

// --- Trace log parsing ---

interface TraceLog {
  address: string;
  topics: string[];
  data: string;
}

/** Recursively collect all emitted logs from a callTracer trace (withLog: true) */
function collectTraceLogs(call: any): TraceLog[] {
  const logs: TraceLog[] = [];
  if (call.logs) {
    for (const log of call.logs) {
      logs.push({ address: log.address.toLowerCase(), topics: log.topics, data: log.data });
    }
  }
  if (call.calls) {
    for (const sub of call.calls) logs.push(...collectTraceLogs(sub));
  }
  return logs;
}

/** Parse Transfer events from trace logs */
function parseTraceTransfers(traceLogs: TraceLog[]): TransferEvent[] {
  const transfers: TransferEvent[] = [];
  for (let i = 0; i < traceLogs.length; i++) {
    const log = traceLogs[i];
    if (log.topics[0] === TRANSFER_TOPIC && log.topics.length >= 3) {
      transfers.push({
        token: log.address,
        from: ("0x" + log.topics[1].slice(26)).toLowerCase(),
        to: ("0x" + log.topics[2].slice(26)).toLowerCase(),
        amount: log.data && log.data.length > 2 ? BigInt(log.data) : 0n,
        logIndex: i,
      });
    }
  }
  return transfers;
}

// --- Split detection ---

function detectSplit(transfers: TransferEvent[], excludeAddrs: Set<string>, poolMap?: Map<string, StoredPool>): boolean {
  const senders = new Set<string>();
  for (const t of transfers) senders.add(t.from);

  const fanOut = new Map<string, Set<string>>();
  const ZERO = "0x0000000000000000000000000000000000000000";
  for (const t of transfers) {
    if (excludeAddrs.has(t.from) || excludeAddrs.has(t.to)) continue;
    if (t.from === ZERO) continue;
    if (!senders.has(t.to) && t.to !== V4_POOL_MANAGER) continue;
    const key = `${t.from}:${t.token}`;
    if (!fanOut.has(key)) fanOut.set(key, new Set());
    fanOut.get(key)!.add(t.to);
  }
  for (const [, recipients] of fanOut) {
    if (recipients.size >= 2) return true;
  }

  // Secondary check: when the fan-out originates from an excluded address
  // (router/user), the primary check misses it. Detect parallel splits by
  // finding 2+ distinct known pools performing the same tokenIn→tokenOut.
  if (poolMap) {
    const swapPairs = new Map<string, number>();
    for (const addr of new Set(transfers.map(t => t.to))) {
      if (addr === ZERO || excludeAddrs.has(addr) || addr === V4_POOL_MANAGER) continue;
      if (!poolMap.has(addr)) continue; // only count known pools
      const ins = transfers.filter(t => t.to === addr && t.from !== ZERO);
      const outs = transfers.filter(t => t.from === addr && t.to !== ZERO);
      if (ins.length === 0 || outs.length === 0) continue;
      const inTokens = new Set(ins.map(t => t.token));
      const outTokens = new Set(outs.map(t => t.token));
      for (const ti of inTokens) {
        for (const to of outTokens) {
          if (ti === to) continue;
          const key = `${ti}:${to}`;
          swapPairs.set(key, (swapPairs.get(key) ?? 0) + 1);
        }
      }
    }
    for (const count of swapPairs.values()) {
      if (count >= 2) return true;
    }
  }

  return false;
}

// --- Pool hop extraction from Transfers ---

interface PoolHop {
  pool: string;
  tokenIn: string;
  tokenOut: string;
}

interface TraceStep {
  pool: string;
  tokenIn: string;
  tokenOut: string;
  amountIn: bigint;
  logIndex: number;
  /** For Balancer V3 buffered swaps: wrappedIn address */
  bufWrappedIn?: string;
  /** For Balancer V3 buffered swaps: Balancer pool address */
  bufPool?: string;
  /** For Balancer V3 buffered swaps: wrappedOut address */
  bufWrappedOut?: string;
  /** True if this step was created by per-transfer splitting */
  perTransferSplit?: boolean;
}

interface ParsedV4Extra {
  id?: string;
  fee?: number;
  hooks?: string;
}

interface SwapStep {
  amountIn: string;
  pools: string[];
  poolTypes: number[];
  tokens: string[];
  extraDatas: string[];
  route: string; // human-readable
}

const V4_POOL_MANAGER = "0x06380c0e0912312b5150364b9dc4542ba0dbbc85";

function parseV4PoolId(extraData?: string): string | undefined {
  if (!extraData) return undefined;
  for (const part of extraData.split(",")) {
    const [key, value] = part.split("=");
    if (key === "id" && value) return value.toLowerCase();
  }
  return undefined;
}

function parseV4Extra(extraData?: string): ParsedV4Extra {
  const parsed: ParsedV4Extra = {};
  if (!extraData) return parsed;

  for (const part of extraData.split(",")) {
    const [key, value] = part.split("=");
    if (!key || value === undefined) continue;
    if (key === "id") parsed.id = value.toLowerCase();
    if (key === "fee") parsed.fee = Number(value);
    if (key === "hooks") parsed.hooks = value.toLowerCase();
  }

  return parsed;
}

function buildV4PoolIdMap(poolMap: Map<string, StoredPool>): Map<string, StoredPool> {
  const v4ById = new Map<string, StoredPool>();
  for (const [, pool] of poolMap) {
    if (pool.poolType !== 9) continue;
    const id = parseV4PoolId(pool.extraData);
    if (id) v4ById.set(id, pool);
  }
  return v4ById;
}

function isPoolAddr(addr: string, poolMap: Map<string, StoredPool>): boolean {
  return poolMap.has(addr) || addr === V4_POOL_MANAGER || addr === WOOPP_ADDRESS || addr === WOOPP_V2_ADDRESS || HASHFLOW_VAULTS.has(addr) || addr === BALANCER_V2_VAULT || addr === CAVALRE_POOL || addr === BENTOBOX;
}

function findPool(addr: string, tokenIn: string, tokenOut: string, poolMap: Map<string, StoredPool>): StoredPool | undefined {
  if (poolMap.has(addr)) return poolMap.get(addr)!;
  // Balancer V3 vault — used for buffered swaps; return a synthetic pool entry
  if (addr === BALANCER_V3_VAULT) {
    return {
      address: BALANCER_V3_VAULT,
      providerName: "balancer_v3_buffered",
      poolType: 11 as any,
      tokens: [tokenIn, tokenOut],
      latestSwapBlock: 0,
    };
  }
  if (addr === BALANCER_V2_VAULT) {
    // Look up the Balancer V2 pool matching this token pair
    for (const [, pool] of poolMap) {
      if (pool.poolType !== 16) continue; // BALANCER_V2
      const toks = pool.tokens.map(t => t.toLowerCase());
      if (toks.includes(tokenIn) && toks.includes(tokenOut)) return pool;
    }
  }
  if (addr === WOOPP_ADDRESS) {
    for (const [, pool] of poolMap) {
      if (pool.poolType !== 5) continue;
      if (pool.address.toLowerCase() !== WOOFI_ROUTER) continue;
      const toks = pool.tokens.map(t => t.toLowerCase());
      if (toks.includes(tokenIn) && toks.includes(tokenOut)) return pool;
    }
  }
  if (addr === WOOPP_V2_ADDRESS) {
    return {
      address: WOOPP_V2_ADDRESS,
      providerName: "woopp_v2",
      poolType: 14 as any, // WOOPP_V2
      tokens: [tokenIn, tokenOut],
      latestSwapBlock: 0,
    };
  }
  // Hashflow vault — simulate via TRANSFER_FROM (pull tokens from vault EOA)
  if (HASHFLOW_VAULTS.has(addr)) {
    return {
      address: addr,
      providerName: "hashflow_rfq",
      poolType: 15 as any,
      tokens: [tokenIn, tokenOut],
      latestSwapBlock: 0,
    };
  }
  if (addr === CAVALRE_POOL) {
    for (const [, pool] of poolMap) {
      if (pool.poolType !== 17) continue; // CAVALRE
      const toks = pool.tokens.map(t => t.toLowerCase());
      if (toks.includes(tokenIn) && toks.includes(tokenOut)) return pool;
    }
  }
  if (addr === BENTOBOX) {
    // BentoBox vault — find the Trident pool matching this token pair
    for (const [, pool] of poolMap) {
      if (pool.poolType !== 20) continue; // TRIDENT
      const toks = pool.tokens.map(t => t.toLowerCase());
      if (toks.includes(tokenIn) && toks.includes(tokenOut)) return pool;
    }
  }
  if (addr === V4_POOL_MANAGER) {
    const candidates: StoredPool[] = [];
    // Normalize WAVAX↔native AVAX: V4 pools store address(0) for native AVAX,
    // but split steps normalize to WAVAX for chaining. Check both forms.
    const altIn = tokenIn === WAVAX ? ZERO_ADDR : tokenIn;
    const altOut = tokenOut === WAVAX ? ZERO_ADDR : tokenOut;
    for (const [, pool] of poolMap) {
      if (pool.poolType !== 9) continue;
      const toks = pool.tokens.map(t => t.toLowerCase());
      const hasIn = toks.includes(tokenIn) || toks.includes(altIn);
      const hasOut = toks.includes(tokenOut) || toks.includes(altOut);
      if (!hasIn || !hasOut) continue;

      const { hooks, fee } = parseV4Extra(pool.extraData);
      if (hooks && hooks !== ZERO_ADDR) continue;
      if (fee !== undefined && fee >= 100000) continue;

      candidates.push(pool);
    }
    candidates.sort((a, b) => b.latestSwapBlock - a.latestSwapBlock);
    return candidates[0];
  }
  return undefined;
}

function extractPoolHops(transfers: TransferEvent[], poolMap: Map<string, StoredPool>): PoolHop[] {
  const hops: PoolHop[] = [];
  const seen = new Set<string>();
  const addrs = new Set<string>();
  for (const t of transfers) { addrs.add(t.from); addrs.add(t.to); }

  // Multi-asset pools that can perform multiple distinct swaps in the same tx.
  // For these, we pair incoming/outgoing transfers by log-index order to extract
  // each swap as a separate hop (e.g. WooPP doing WETH.e→USDt then BTC.b→USDC).
  const MULTI_SWAP_POOLS = new Set([WOOPP_ADDRESS, WOOPP_V2_ADDRESS, BALANCER_V2_VAULT, CAVALRE_POOL]);

  for (const addr of addrs) {
    if (addr === ZERO_ADDR || addr === V4_POOL_MANAGER || seen.has(addr)) continue;
    if (!poolMap.has(addr) && addr !== WOOPP_ADDRESS && addr !== WOOPP_V2_ADDRESS && !HASHFLOW_VAULTS.has(addr) && addr !== BALANCER_V2_VAULT && addr !== CAVALRE_POOL && addr !== BENTOBOX) continue;
    const incoming = transfers.filter(t => t.to === addr && t.from !== ZERO_ADDR);
    const outgoing = transfers.filter(t => t.from === addr && t.to !== ZERO_ADDR);
    if (incoming.length > 0 && outgoing.length > 0) {
      // For multi-asset pools with multiple swaps, pair transfers by log-index
      // order to emit one hop per distinct swap.
      if (MULTI_SWAP_POOLS.has(addr) && incoming.length > 1 && outgoing.length > 1) {
        // Sort by logIndex and pair: each incoming[i] with outgoing[i]
        incoming.sort((a, b) => a.logIndex - b.logIndex);
        outgoing.sort((a, b) => a.logIndex - b.logIndex);
        const pairCount = Math.min(incoming.length, outgoing.length);
        for (let i = 0; i < pairCount; i++) {
          const tokenIn = incoming[i].token;
          const tokenOut = outgoing[i].token;
          if (tokenIn === tokenOut) continue; // skip no-op
          hops.push({ pool: addr, tokenIn, tokenOut });
        }
        seen.add(addr);
        continue;
      }
      const tokenIn = incoming[0].token;
      const tokenOut = outgoing[0].token;
      // Skip ERC4626 vault hops where input and output tokens are the same (wrapping/unwrapping)
      // — these are handled separately by the Balancer V3 buffered swap detection
      const storedPool = poolMap.get(addr);
      if (tokenIn === tokenOut && storedPool?.poolType === 10) continue;
      seen.add(addr);
      hops.push({ pool: addr, tokenIn, tokenOut });
    }
  }

  // Sort hops by token-chain order instead of log index.
  // Flash-swap pools (algebra, uniV3) emit output transfers BEFORE input
  // transfers, so log-index sorting produces wrong order.
  // Build the longest chain: find start candidates (tokenIn not produced by
  // any other hop's tokenOut), then greedily follow tokenOut→tokenIn links.
  if (hops.length > 1) {
    const producedTokens = new Set(hops.map(h => h.tokenOut));
    const startCandidates = hops.filter(h => !producedTokens.has(h.tokenIn));

    let bestChain: PoolHop[] = [];
    const tryBuild = (start: PoolHop) => {
      const chain: PoolHop[] = [start];
      const used = new Set<string>([start.pool + start.tokenIn + start.tokenOut]);
      let current = start;
      while (chain.length < hops.length) {
        const next = hops.find(
          h => h.tokenIn === current.tokenOut && !used.has(h.pool + h.tokenIn + h.tokenOut)
        );
        if (!next) break;
        chain.push(next);
        used.add(next.pool + next.tokenIn + next.tokenOut);
        current = next;
      }
      return chain;
    };

    // Try each start candidate and pick the longest chain
    for (const s of startCandidates.length > 0 ? startCandidates : hops) {
      const chain = tryBuild(s);
      if (chain.length > bestChain.length) bestChain = chain;
      if (bestChain.length === hops.length) break;
    }

    // If the chain covers all hops, use it; otherwise fall back to log-index sort
    if (bestChain.length === hops.length) {
      return bestChain;
    }
  }

  // Fallback: sort by log index (works for most cases except flash-swap pools)
  hops.sort((a, b) => {
    const aIdx = transfers.find(t => t.to === a.pool)?.logIndex ?? 0;
    const bIdx = transfers.find(t => t.to === b.pool)?.logIndex ?? 0;
    return aIdx - bIdx;
  });
  return hops;
}

/** Extract Wombat swap steps from trace logs by detecting the Wombat Swap event.
 *  The Wombat Swap event encodes fromToken and toToken in the data field (first 64 bytes).
 *  The actual amounts are determined from surrounding Transfer events. */
function extractWombatTraceSteps(
  traceLogs: TraceLog[],
  transfers: TransferEvent[],
  poolMap: Map<string, StoredPool>,
): TraceStep[] {
  const steps: TraceStep[] = [];

  for (let i = 0; i < traceLogs.length; i++) {
    const log = traceLogs[i];
    if (log.topics[0] !== WOMBAT_SWAP_TOPIC) continue;

    const poolAddr = log.address.toLowerCase();
    if (!poolMap.has(poolAddr)) continue;

    // Wombat Swap event data: fromToken (address, padded) + toToken (address, padded) + fromAmount + toAmount
    const data = log.data;
    if (data.length < 130) continue; // at least 2 x 32 bytes
    const fromToken = ("0x" + data.slice(26, 66)).toLowerCase();
    const toToken = ("0x" + data.slice(90, 130)).toLowerCase();

    // Find the amount of fromToken transferred to the Wombat router/pool area around this event
    // The amountIn is determined from the Transfer log of fromToken closest before this Wombat event
    let amountIn = 0n;
    for (let j = i - 1; j >= 0; j--) {
      const tl = traceLogs[j];
      if (tl.topics[0] !== TRANSFER_TOPIC) continue;
      if (tl.address.toLowerCase() !== fromToken) continue;
      amountIn = BigInt(tl.data);
      break;
    }
    if (amountIn === 0n) continue;

    steps.push({
      pool: poolAddr,
      tokenIn: fromToken,
      tokenOut: toToken,
      amountIn,
      logIndex: i,
    });
  }

  return steps;
}

/** Extract Synapse stableswap trace steps from TokenSwap events. */
function extractSynapseTraceSteps(
  traceLogs: TraceLog[],
  transfers: TransferEvent[],
  poolMap: Map<string, StoredPool>,
): TraceStep[] {
  const steps: TraceStep[] = [];
  for (let i = 0; i < traceLogs.length; i++) {
    const log = traceLogs[i];
    if (log.topics[0] !== SYNAPSE_TOKEN_SWAP_TOPIC) continue;
    const poolAddr = log.address.toLowerCase();
    const tokens = SYNAPSE_TOKENS[poolAddr];
    if (!tokens) continue;
    if (!poolMap.has(poolAddr)) continue;
    const data = log.data;
    if (data.length < 2 + 64 * 4) continue;
    const tokensSold = BigInt("0x" + data.slice(2, 66));
    const tokensBought = BigInt("0x" + data.slice(66, 130));
    const soldId = Number(BigInt("0x" + data.slice(130, 194)));
    const boughtId = Number(BigInt("0x" + data.slice(194, 258)));
    if (soldId >= tokens.length || boughtId >= tokens.length) continue;
    if (tokensSold <= 0n || tokensBought <= 0n) continue;
    steps.push({
      pool: poolAddr,
      tokenIn: tokens[soldId],
      tokenOut: tokens[boughtId],
      amountIn: tokensSold,
      logIndex: i,
    });
  }
  return steps;
}

/** Detect Balancer V3 buffered swaps: underlying_in → wrap → vault swap → unwrap → underlying_out.
 *  Only detects full 3-step buffered swaps where both input and output are underlying tokens
 *  that get wrapped/unwrapped through the vault's internal ERC4626 buffers.
 */
const BALANCER_V3_SWAP_TOPIC = "0x0874b2d545cb271cdbda4e093020c452328b24af12382ed62c4d00f5c26709db";

function extractBalancerBufferedTraceSteps(
  traceLogs: TraceLog[],
  transfers: TransferEvent[],
  poolMap: Map<string, StoredPool>,
): TraceStep[] {
  const steps: TraceStep[] = [];

  for (let i = 0; i < traceLogs.length; i++) {
    const log = traceLogs[i];
    if (log.address.toLowerCase() !== BALANCER_V3_VAULT) continue;
    if (log.topics[0] !== BALANCER_V3_SWAP_TOPIC) continue;
    if (log.topics.length < 4) continue;

    const balPool = ("0x" + log.topics[1].slice(26)).toLowerCase();
    const swapTokenIn = ("0x" + log.topics[2].slice(26)).toLowerCase();
    const swapTokenOut = ("0x" + log.topics[3].slice(26)).toLowerCase();

    const stored = poolMap.get(balPool);
    if (!stored || stored.poolType !== 6) continue;

    // For a full buffered swap: the swapTokenIn should be a wrapped ERC4626 token,
    // NOT a direct pool token that's also an underlying somewhere.
    // Detect by checking if the input token (swapTokenIn) is NOT the same as any token
    // that was transferred TO the vault before this event.

    // Find the underlying token transferred TO the vault before this swap event
    let underlyingIn: string | undefined;
    let amountIn = 0n;
    let transferIdx = i;
    for (let j = i - 1; j >= Math.max(0, i - 15); j--) {
      const tl = traceLogs[j];
      if (tl.topics[0] !== TRANSFER_TOPIC) continue;
      const to = ("0x" + tl.topics[2].slice(26)).toLowerCase();
      const token = tl.address.toLowerCase();
      // Must be a transfer TO the vault of a different token than the swap tokens
      if (to === BALANCER_V3_VAULT && token !== swapTokenIn && token !== swapTokenOut) {
        underlyingIn = token;
        amountIn = BigInt(tl.data);
        transferIdx = j;
        break;
      }
    }
    if (underlyingIn && amountIn > 0n) {
      // Fully buffered swap: both input and output go through ERC4626 wrap/unwrap
      // Find the underlying output token transferred FROM the vault after this swap event
      let underlyingOut: string | undefined;
      for (let j = i + 1; j < Math.min(traceLogs.length, i + 15); j++) {
        const tl = traceLogs[j];
        if (tl.topics[0] !== TRANSFER_TOPIC) continue;
        const from = ("0x" + tl.topics[1].slice(26)).toLowerCase();
        const token = tl.address.toLowerCase();
        if (from === BALANCER_V3_VAULT && token !== swapTokenIn && token !== swapTokenOut) {
          underlyingOut = token;
          break;
        }
      }
      if (!underlyingOut) continue;
      if (underlyingIn === underlyingOut) continue;

      steps.push({
        pool: BALANCER_V3_VAULT,
        tokenIn: underlyingIn,
        tokenOut: underlyingOut,
        amountIn,
        logIndex: transferIdx,
        bufWrappedIn: swapTokenIn,
        bufPool: balPool,
        bufWrappedOut: swapTokenOut,
      });
      continue;
    }

    // Half-buffered swap: input token IS a direct pool token (no wrapping needed),
    // but output goes through ERC4626 unwrap. E.g. stAVAX → [BalV3 pool] → waAvaSAVAX → sAVAX.
    // Detect by finding swapTokenIn transferred TO the vault before the swap event.
    let halfAmountIn = 0n;
    let halfTransferIdx = i;
    for (let j = i - 1; j >= Math.max(0, i - 15); j--) {
      const tl = traceLogs[j];
      if (tl.topics[0] !== TRANSFER_TOPIC) continue;
      const to = ("0x" + tl.topics[2].slice(26)).toLowerCase();
      const token = tl.address.toLowerCase();
      if (to === BALANCER_V3_VAULT && token === swapTokenIn) {
        halfAmountIn = BigInt(tl.data);
        halfTransferIdx = j;
        break;
      }
    }
    if (halfAmountIn === 0n) continue;

    // Check if output side has an ERC4626 unwrap: underlying token transferred FROM vault after swap
    let halfUnderlyingOut: string | undefined;
    for (let j = i + 1; j < Math.min(traceLogs.length, i + 15); j++) {
      const tl = traceLogs[j];
      if (tl.topics[0] !== TRANSFER_TOPIC) continue;
      const from = ("0x" + tl.topics[1].slice(26)).toLowerCase();
      const token = tl.address.toLowerCase();
      if (from === BALANCER_V3_VAULT && token !== swapTokenIn && token !== swapTokenOut) {
        halfUnderlyingOut = token;
        break;
      }
    }
    if (!halfUnderlyingOut) continue;

    // Emit as a BalV3 pool swap (type 6): swapTokenIn → swapTokenOut.
    // buildPayload's findTailHops will chain the ERC4626 unwrap (swapTokenOut → underlyingOut).
    steps.push({
      pool: balPool,
      tokenIn: swapTokenIn,
      tokenOut: swapTokenOut,
      amountIn: halfAmountIn,
      logIndex: halfTransferIdx,
    });
  }

  return steps;
}

function extractV4TraceSteps(traceLogs: TraceLog[], v4PoolIdMap: Map<string, StoredPool>): TraceStep[] {
  const steps: TraceStep[] = [];

  for (let i = 0; i < traceLogs.length; i++) {
    const log = traceLogs[i];
    if (log.address !== V4_POOL_MANAGER) continue;
    if (log.topics[0] !== V4_SWAP_TOPIC || !log.topics[1]) continue;

    const pool = v4PoolIdMap.get(log.topics[1].toLowerCase());
    if (!pool || pool.tokens.length < 2) continue;

    let amount0: bigint;
    let amount1: bigint;
    try {
      [amount0, amount1] = decodeAbiParameters(
        [
          { type: "int128" },
          { type: "int128" },
          { type: "uint160" },
          { type: "uint128" },
          { type: "int24" },
          { type: "uint24" },
        ],
        log.data as Hex,
      );
    } catch {
      continue;
    }

    const token0 = pool.tokens[0].toLowerCase();
    const token1 = pool.tokens[1].toLowerCase();

    if (amount0 < 0n && amount1 >= 0n) {
      steps.push({
        pool: pool.address.toLowerCase(),
        tokenIn: token0,
        tokenOut: token1,
        amountIn: -amount0,
        logIndex: i,
      });
    } else if (amount1 < 0n && amount0 >= 0n) {
      steps.push({
        pool: pool.address.toLowerCase(),
        tokenIn: token1,
        tokenOut: token0,
        amountIn: -amount1,
        logIndex: i,
      });
    }
  }

  return steps;
}

// --- Split step extraction ---

function extractSplitSteps(
  transfers: TransferEvent[],
  poolMap: Map<string, StoredPool>,
  traceLogs: TraceLog[],
  v4PoolIdMap: Map<string, StoredPool>,
): TraceStep[] {
  const steps: TraceStep[] = [];

  const addrs = new Set<string>();
  for (const t of transfers) { addrs.add(t.from); addrs.add(t.to); }

  for (const addr of addrs) {
    if (addr === ZERO_ADDR || addr === V4_POOL_MANAGER) continue;
    if (!isPoolAddr(addr, poolMap)) continue;

    const incoming = transfers.filter(t => t.to === addr && t.from !== ZERO_ADDR);
    const outgoing = transfers.filter(t => t.from === addr && t.to !== ZERO_ADDR);
    if (incoming.length === 0 || outgoing.length === 0) continue;

    // Group incoming transfers by token
    const inByToken = new Map<string, TransferEvent[]>();
    for (const t of incoming) {
      const arr = inByToken.get(t.token);
      if (arr) arr.push(t);
      else inByToken.set(t.token, [t]);
    }

    const outByToken = new Map<string, bigint>();
    for (const t of outgoing) {
      outByToken.set(t.token, (outByToken.get(t.token) ?? 0n) + t.amount);
    }

    // Per-transfer split: when a pool receives multiple transfers of the same token
    // AND produces multiple output transfers of the same output token, create separate
    // steps for each input transfer. This handles cases where the aggregator sends
    // the input in batches, each destined for a different downstream route.
    const outCountByToken = new Map<string, number>();
    for (const t of outgoing) {
      outCountByToken.set(t.token, (outCountByToken.get(t.token) ?? 0) + 1);
    }

    for (const [tokenIn, inTransfers] of inByToken) {
      // Count how many distinct output tokens this input produces (excluding self-token).
      // Virtual pools (WooPP) can route one input to multiple outputs; in that case
      // divide the input proportionally based on output amounts.
      const outputTokensForInput = [...outByToken.keys()].filter(t => t !== tokenIn);

      for (const tokenOut of outputTokensForInput) {
        const outCount = outCountByToken.get(tokenOut) ?? 0;
        // Split when multiple input transfers AND multiple output transfers of
        // the corresponding token, indicating distinct batched swaps.
        // Case 1: input count matches output count exactly (1:1 mapping).
        // Case 2: pool handles multiple input tokens producing the same output
        //   (e.g., WooPP: WAVAX→WETH.e + USDC→WETH.e). Split when the total
        //   input transfer count across ALL input tokens equals the output count,
        //   indicating each input transfer maps to one output transfer.
        const totalInCountForOutput = [...inByToken.entries()]
          .filter(([t]) => t !== tokenOut)
          .reduce((sum, [, arr]) => sum + arr.length, 0);
        const shouldSplit = inTransfers.length > 1 && outCount > 1 &&
          (inTransfers.length === outCount || totalInCountForOutput === outCount);
        if (shouldSplit) {
          for (const t of inTransfers) {
            steps.push({ pool: addr, tokenIn, tokenOut, amountIn: t.amount, logIndex: t.logIndex, perTransferSplit: true });
          }
        } else {
          let total = inTransfers.reduce((s, t) => s + t.amount, 0n);
          const minIdx = Math.min(...inTransfers.map(t => t.logIndex));
          // Note: when a virtual pool (e.g. WooPP) routes one input to multiple outputs,
          // each step gets the full input amount. The test harness handles this via
          // dependency-aware flat ordering and tolerances.
          steps.push({ pool: addr, tokenIn, tokenOut, amountIn: total, logIndex: minIdx });
        }
      }
    }
  }

  // Add V4 steps. When a V4 pool uses native AVAX (address(0)), normalize
  // the token to WAVAX for correct chaining with other pools, and add a
  // wrapNative=1 flag so the router auto-wraps WAVAX<->AVAX during the swap.
  for (const v4 of extractV4TraceSteps(traceLogs, v4PoolIdMap)) {
    const needsWrap = v4.tokenIn === ZERO_ADDR || v4.tokenOut === ZERO_ADDR;
    const stored = poolMap.get(v4.pool);
    if (needsWrap && stored && stored.extraData) {
      // Clone step with normalized tokens and wrapNative flag in extraData
      steps.push({
        ...v4,
        tokenIn: v4.tokenIn === ZERO_ADDR ? WAVAX : v4.tokenIn,
        tokenOut: v4.tokenOut === ZERO_ADDR ? WAVAX : v4.tokenOut,
        // Override pool address so hopToSwapFields uses the modified extraData
        _wrapNativeExtraData: stored.extraData + ",wrapNative=1",
      } as any);
    } else {
      steps.push(v4);
    }
  }
  steps.sort((a, b) => a.logIndex - b.logIndex);
  return steps;
}

// --- Tail hop finder for extending split steps ---

/** Find a short (1-2 hop) path from intermediateToken to targetToken.
 *  Prefers Balancer V3 swap + ERC4626 unwrap routes (trace-verified paths),
 *  then falls back to 1-hop pool scanning. */
function findTailHops(
  intermediateToken: string,
  targetToken: string,
  poolMap: Map<string, StoredPool>,
  traceLogs?: TraceLog[],
): { pool: string; poolType: number; extraData: string; providerName: string; tokenOut: string }[] {
  // Build underlying→wrapped map for ERC4626 vault lookup
  const underlyingToWrapped = new Map<string, string>();
  for (const v of ERC4626_VAULTS) {
    underlyingToWrapped.set(v.tokens[0].toLowerCase(), v.address.toLowerCase());
  }

  // Strategy 1: Find a Balancer V3 swap involving intermediateToken from the trace,
  // followed by an ERC4626 unwrap to targetToken.
  if (traceLogs) {
    for (let i = 0; i < traceLogs.length; i++) {
      const log = traceLogs[i];
      if (log.address.toLowerCase() !== BALANCER_V3_VAULT) continue;
      if (log.topics[0] !== BALANCER_V3_SWAP_TOPIC) continue;
      if (log.topics.length < 4) continue;

      const balPool = ("0x" + log.topics[1].slice(26)).toLowerCase();
      const swapTokenIn = ("0x" + log.topics[2].slice(26)).toLowerCase();
      const swapTokenOut = ("0x" + log.topics[3].slice(26)).toLowerCase();

      // Strategy 1a: intermediateToken is directly the input to this Balancer swap (half-buffered)
      if (swapTokenIn === intermediateToken) {
        // Check if balPool is in poolMap
        const stored = poolMap.get(balPool);
        if (!stored || stored.poolType !== 6) continue;

        // Now check if swapTokenOut can be unwrapped to targetToken via ERC4626
        const erc4626 = poolMap.get(swapTokenOut.toLowerCase());
        if (erc4626 && erc4626.poolType === 10) {
          const toks = erc4626.tokens.map(t => t.toLowerCase());
          if (toks.includes(targetToken)) {
            const f1 = hopToSwapFields({ pool: balPool, tokenIn: intermediateToken, tokenOut: swapTokenOut }, poolMap);
            const f2 = hopToSwapFields({ pool: swapTokenOut, tokenIn: swapTokenOut, tokenOut: targetToken }, poolMap);
            if (f1 && f2) return [{ ...f1, tokenOut: swapTokenOut }, { ...f2, tokenOut: targetToken }];
          }
        }
        continue;
      }

      // Strategy 1b: intermediateToken is the underlying of a wrapped swapTokenIn (fully buffered).
      // e.g. AUSD→USDC: vault swap is waAvaAUSD→waAvaUSDC, but we're routing AUSD→USDC.
      const wrappedIn = underlyingToWrapped.get(intermediateToken);
      const wrappedOut = underlyingToWrapped.get(targetToken);
      if (wrappedIn && wrappedOut && swapTokenIn === wrappedIn && swapTokenOut === wrappedOut) {
        // This BalV3 swap is actually a buffered AUSD→USDC (or similar). Look for the type 11 edge.
        const bufferedEdgeKey = `${balPool}:${intermediateToken}:${targetToken}`;
        const bufferedStored = poolMap.get(bufferedEdgeKey);
        if (bufferedStored && bufferedStored.poolType === 11 && bufferedStored.extraData) {
          const f = hopToSwapFields({ pool: bufferedEdgeKey, tokenIn: intermediateToken, tokenOut: targetToken }, poolMap);
          if (f) return [{ ...f, tokenOut: targetToken }];
        }
      }
    }
  }

  // Strategy 1.5: Detect the actual pool from trace Transfer events.
  // Look for intermediateToken transferred TO a known pool and targetToken FROM that pool.
  if (traceLogs) {
    const tracePoolCandidates = new Map<string, { hasIn: boolean; hasOut: boolean }>();
    for (const log of traceLogs) {
      if (log.topics[0] !== TRANSFER_TOPIC || log.topics.length < 3) continue;
      const token = log.address.toLowerCase();
      const to = ("0x" + log.topics[2].slice(26)).toLowerCase();
      const from = ("0x" + log.topics[1].slice(26)).toLowerCase();
      if (token === intermediateToken && poolMap.has(to)) {
        const entry = tracePoolCandidates.get(to) ?? { hasIn: false, hasOut: false };
        entry.hasIn = true;
        tracePoolCandidates.set(to, entry);
      }
      if (token === targetToken && poolMap.has(from)) {
        const entry = tracePoolCandidates.get(from) ?? { hasIn: false, hasOut: false };
        entry.hasOut = true;
        tracePoolCandidates.set(from, entry);
      }
    }
    for (const [addr, { hasIn, hasOut }] of tracePoolCandidates) {
      if (hasIn && hasOut) {
        const f = hopToSwapFields({ pool: addr, tokenIn: intermediateToken, tokenOut: targetToken }, poolMap);
        if (f) return [{ ...f, tokenOut: targetToken }];
      }
    }
  }

  // Strategy 2: Direct 1-hop via known pools.
  // Priority: active AMM (UV3/Algebra/WooFi/LFJ/etc., latestSwapBlock > AMM_RECENT_THRESHOLD) >
  //           buffered BalV3 (type 11) > regular BalV3 (type 6) > ERC4626 (type 10) > stale AMM.
  // Active AMMs are preferred over type 11 because type 11 synthetic edges can fail for small
  // amounts when buffer liquidity is insufficient. But type 11 beats stale/inactive AMMs
  // (e.g. AUSD→USDC where no liquid AMM pool exists with recent activity).
  const AMM_TYPES = new Set([0, 1, 3, 4, 5, 7, 8, 9, 12, 13, 16, 17, 18, 19, 20]); // +KyberDMM, Synapse, Trident
  // Priority score for AMM types (higher = preferred when latestSwapBlock is equal).
  // UV3 (0) and Algebra (1) are most reliable; Platypus/Wombat preferred for stable pairs.
  const AMM_TYPE_PRIORITY: Record<number, number> = { 0: 100, 1: 90, 3: 80, 16: 78, 19: 76, 13: 75, 12: 74, 9: 70, 18: 68, 20: 66, 17: 65, 7: 60, 5: 50, 8: 40, 4: 30 };
  // Blocks ~6 months before latest benchmarked tx; pools with swaps more recent than this are "active"
  const AMM_RECENT_THRESHOLD = 78_000_000;
  type AmmCandidate = { pool: string; poolType: number; extraData: string; providerName: string; tokenOut: string; block: number; priority: number };
  let bestActiveAmm: AmmCandidate | undefined;
  let bestStaleAmm: AmmCandidate | undefined;
  let bestBuffered: { pool: string; poolType: number; extraData: string; providerName: string; tokenOut: string } | undefined;
  let bestBalV3: { pool: string; poolType: number; extraData: string; providerName: string; tokenOut: string } | undefined;
  let bestErc4626: { pool: string; poolType: number; extraData: string; providerName: string; tokenOut: string } | undefined;
  for (const [addr, stored] of poolMap) {
    const toks = stored.tokens.map(t => t.toLowerCase());
    if (toks.includes(intermediateToken) && toks.includes(targetToken)) {
      const f = hopToSwapFields({ pool: addr, tokenIn: intermediateToken, tokenOut: targetToken }, poolMap);
      if (f) {
        if (AMM_TYPES.has(stored.poolType)) {
          const priority = AMM_TYPE_PRIORITY[stored.poolType] ?? 0;
          const candidate: AmmCandidate = { ...f, tokenOut: targetToken, block: stored.latestSwapBlock, priority };
          if (stored.latestSwapBlock >= AMM_RECENT_THRESHOLD) {
            // Among active AMMs, prefer higher type priority; break ties by latest block
            if (!bestActiveAmm ||
                priority > bestActiveAmm.priority ||
                (priority === bestActiveAmm.priority && stored.latestSwapBlock > bestActiveAmm.block)) {
              bestActiveAmm = candidate;
            }
          } else {
            if (!bestStaleAmm ||
                stored.latestSwapBlock > bestStaleAmm.block ||
                (stored.latestSwapBlock === bestStaleAmm.block && priority > bestStaleAmm.priority)) {
              bestStaleAmm = candidate;
            }
          }
        } else if (stored.poolType === 11) {
          if (!bestBuffered) bestBuffered = { ...f, tokenOut: targetToken };
        } else if (stored.poolType === 6) {
          if (!bestBalV3) bestBalV3 = { ...f, tokenOut: targetToken };
        } else if (stored.poolType === 10) {
          if (!bestErc4626) bestErc4626 = { ...f, tokenOut: targetToken };
        }
      }
    }
  }
  // Strategy 2.5: ERC4626 unwrap + AMM for wrapped tokens in boosted pools.
  // e.g. waAvaWAVAX→WAVAX→USDC instead of direct BalV3 waAvaWAVAX→USDC (which reverts).
  if (!bestActiveAmm && !bestBuffered) {
    const erc4626Vault = poolMap.get(intermediateToken);
    if (erc4626Vault && erc4626Vault.poolType === 10) {
      const underlying = erc4626Vault.tokens.map(t => t.toLowerCase()).find(t => t !== intermediateToken);
      if (underlying && underlying !== targetToken) {
        const unwrapF = hopToSwapFields({ pool: intermediateToken, tokenIn: intermediateToken, tokenOut: underlying }, poolMap);
        if (unwrapF) {
          const subHops = findTailHops(underlying, targetToken, poolMap, traceLogs);
          if (subHops.length > 0 && subHops.length <= 2) {
            return [{ ...unwrapF, tokenOut: underlying }, ...subHops];
          }
        }
      }
    }
  }

  // Return best candidate: active AMM > buffered > BalV3 > ERC4626 > stale AMM
  const s2best = bestActiveAmm ?? bestBuffered ?? bestBalV3 ?? bestErc4626 ?? bestStaleAmm;
  if (s2best) return [{ pool: s2best.pool, poolType: s2best.poolType, extraData: s2best.extraData, providerName: s2best.providerName, tokenOut: s2best.tokenOut }];

  // Strategy 3: 2-hop — intermediateToken → midToken → targetToken via ERC4626 unwrap
  for (const [addr1, stored1] of poolMap) {
    const toks1 = stored1.tokens.map(t => t.toLowerCase());
    if (!toks1.includes(intermediateToken)) continue;
    if (stored1.poolType !== 6) continue; // Prefer Balancer V3 for first hop
    for (const midToken of toks1) {
      if (midToken === intermediateToken) continue;
      const erc4626 = poolMap.get(midToken);
      if (erc4626 && erc4626.poolType === 10) {
        const erc4626Toks = erc4626.tokens.map(t => t.toLowerCase());
        if (erc4626Toks.includes(targetToken)) {
          const f1 = hopToSwapFields({ pool: addr1, tokenIn: intermediateToken, tokenOut: midToken }, poolMap);
          const f2 = hopToSwapFields({ pool: midToken, tokenIn: midToken, tokenOut: targetToken }, poolMap);
          if (f1 && f2) return [{ ...f1, tokenOut: midToken }, { ...f2, tokenOut: targetToken }];
        }
      }
    }
  }

  return [];
}

// --- Payload generation ---

interface Payload {
  block: number;
  txHash: string;
  source: string;
  isSplit: boolean;
  inputToken: string;
  outputToken: string;
  amountIn: string;
  expectedOut: string;
  gasUsed: number;
  supported: boolean;
  unsupportedReason?: string;
  pools?: string[];
  poolTypes?: number[];
  tokens?: string[];
  extraDatas?: string[];
  steps?: SwapStep[];
  route?: string;
}

function hopToSwapFields(hop: { pool: string; tokenIn: string; tokenOut: string; bufWrappedIn?: string; bufPool?: string; bufWrappedOut?: string; rfqOutputAmount?: bigint; _wrapNativeExtraData?: string }, poolMap: Map<string, StoredPool>):
  { pool: string; poolType: number; extraData: string; providerName: string } | undefined {
  const stored = findPool(hop.pool, hop.tokenIn, hop.tokenOut, poolMap);
  if (!stored) return undefined;
  if (stored.poolType === 9) {
    // Use wrapNative-augmented extraData when the V4 step was normalized from native AVAX to WAVAX
    const extraData = (hop as any)._wrapNativeExtraData ?? stored.extraData ?? "";
    return { pool: "0x06380C0e0912312B5150364B9DC4542BA0DbBc85", poolType: 9, extraData, providerName: stored.providerName };
  }
  // Balancer V3 buffered: encode extraData as abi.encode(wrappedIn, pool, wrappedOut)
  if (stored.poolType === 11 && hop.bufWrappedIn && hop.bufPool && hop.bufWrappedOut) {
    const extraData = encodeAbiParameters(
      [{ type: "address" }, { type: "address" }, { type: "address" }],
      [hop.bufWrappedIn as Hex, hop.bufPool as Hex, hop.bufWrappedOut as Hex],
    );
    return { pool: BALANCER_V3_VAULT, poolType: 11, extraData, providerName: "balancer_v3_buffered" };
  }
  // Balancer V3 buffered from generateBufferedEdges: extraData is already in KV format
  if (stored.poolType === 11 && stored.extraData) {
    return { pool: BALANCER_V3_VAULT, poolType: 11, extraData: stored.extraData, providerName: "balancer_v3_buffered" };
  }
  // TRANSFER_FROM (RFQ vault): encode output amount as extraData
  if (stored.poolType === 15 && hop.rfqOutputAmount !== undefined) {
    const extraData = encodeAbiParameters([{ type: "uint256" }], [hop.rfqOutputAmount]);
    return { pool: stored.address, poolType: 15, extraData, providerName: stored.providerName };
  }
  // Balancer V2: pass through poolId extraData
  if (stored.poolType === 16 && stored.extraData) {
    return { pool: stored.address, poolType: 16, extraData: stored.extraData, providerName: stored.providerName };
  }
  if (stored.poolType === 20 && stored.extraData) { // TRIDENT
    return { pool: stored.address, poolType: 20, extraData: stored.extraData, providerName: stored.providerName };
  }
  if (stored.poolType === 19) { // SYNAPSE
    const synapseTokens = SYNAPSE_TOKENS[stored.address.toLowerCase()];
    if (synapseTokens) {
      const fromIdx = synapseTokens.indexOf(hop.tokenIn.toLowerCase());
      const toIdx = synapseTokens.indexOf(hop.tokenOut.toLowerCase());
      if (fromIdx >= 0 && toIdx >= 0) {
        return { pool: stored.address, poolType: 19, extraData: `from=${fromIdx},to=${toIdx}`, providerName: stored.providerName };
      }
    }
    return undefined;
  }
  if (stored.poolType === 17) { // CAVALRE
    return { pool: stored.address, poolType: 17, extraData: stored.address, providerName: stored.providerName };
  }
  // UniV2-style pools with non-standard fees: encode feeBps in extraData
  const CUSTOM_FEE_PROVIDERS: Record<string, string> = {
    "oliveswap": "fee=9980",  // 0.2% fee (vs standard 0.3%)
    "vapordex": "fee=9971",   // 0.29% fee
    "lydia": "fee=9980",      // 0.2% fee (Lydia pair uses mul(2) not mul(3))
    "hakuswap": "fee=9980",   // 0.2% fee (HakuswapPair uses mul(2))
    "thorus": "fee=9990",     // 0.1% fee (ThorusPair uses mul(1) not mul(3))
  };
  if (stored.poolType === 8 && CUSTOM_FEE_PROVIDERS[stored.providerName]) {
    return { pool: stored.address, poolType: stored.poolType, extraData: CUSTOM_FEE_PROVIDERS[stored.providerName], providerName: stored.providerName };
  }
  return { pool: stored.address, poolType: stored.poolType, extraData: "", providerName: stored.providerName };
}

function buildPayload(
  txHash: string,
  blockNumber: number,
  source: string,
  isSplit: boolean,
  inputToken: string,
  outputToken: string,
  inputAmount: bigint,
  outputAmount: bigint,
  gasUsed: bigint,
  hops: PoolHop[],
  poolMap: Map<string, StoredPool>,
  splitSteps: TraceStep[] = [],
  traceLogs: TraceLog[] = [],
  transfers: TransferEvent[] = [],
): Payload {
  const base: Payload = {
    block: blockNumber,
    txHash,
    source,
    isSplit,
    inputToken,
    outputToken,
    amountIn: inputAmount.toString(),
    expectedOut: outputAmount.toString(),
    gasUsed: Number(gasUsed),
    supported: false,
  };

  if (isSplit) {
    if (splitSteps.length === 0) {
      base.unsupportedReason = "split route: no pool swaps detected";
      base.route = `SPLIT: ${hops.length} pools`;
      return base;
    }

    const missingPools: string[] = [];
    for (const step of splitSteps) {
      if (!findPool(step.pool, step.tokenIn, step.tokenOut, poolMap)) missingPools.push(step.pool);
    }
    if (missingPools.length > 0) {
      base.unsupportedReason = `missing pools: ${[...new Set(missingPools)].join(", ")}`;
      base.route = `SPLIT(${splitSteps.length}): ${splitSteps.map(s => `${tok(s.tokenIn)}→${tok(s.tokenOut)}`).join(", ")}`;
      return base;
    }

    // Build a set of all step input tokens, so we know which intermediates
    // are consumed by other steps (and thus shouldn't be tail-extended).
    // This will be rebuilt after chaining to exclude consumed downstream steps.
    let stepInputTokens = new Set(splitSteps.map(s => s.tokenIn));

    // Transfer-based chaining: chain upstream→downstream steps when the intermediate
    // token flows from upstream pool to downstream pool (directly or via non-pool
    // intermediary like the router), AND the downstream pool receives that intermediate
    // ONLY from this upstream pool path (no other independent sources).
    const poolAddrs = new Set(splitSteps.map(s => s.pool));
    // V4 pools use virtual addresses in splitSteps, but tokens are transferred to V4_POOL_MANAGER.
    // Include the manager address so tokenFlows tracks transfers to V4 pools.
    const hasV4Steps = splitSteps.some(s => {
      const sp = findPool(s.pool, s.tokenIn, s.tokenOut, poolMap);
      return sp && sp.poolType === 9;
    });
    if (hasV4Steps) poolAddrs.add(V4_POOL_MANAGER);
    const chainedDownstream = new Set<number>(); // indices of steps consumed by chaining
    const chainMap = new Map<number, number>(); // upstream index → downstream index

    // Build a map: for each (token, recipient pool), track the set of source pools
    // that ultimately feed it (tracing through non-pool intermediaries)
    const tokenFlows = new Map<string, Set<string>>(); // "token:toPool" → set of from-pools
    for (const t of transfers) {
      if (t.from === ZERO_ADDR || t.to === ZERO_ADDR) continue;
      if (!poolAddrs.has(t.to)) continue; // only care about transfers TO pools

      if (poolAddrs.has(t.from)) {
        // Direct pool→pool transfer
        const key = `${t.token}:${t.to}`;
        let s = tokenFlows.get(key);
        if (!s) { s = new Set(); tokenFlows.set(key, s); }
        s.add(t.from);
      } else {
        // Transfer from non-pool (e.g. router) → trace back to see which pool
        // sent this token to the non-pool intermediary
        const feeders = transfers.filter(
          f => f.to === t.from && f.token === t.token && f.from !== ZERO_ADDR && f.logIndex < t.logIndex
        );
        const key = `${t.token}:${t.to}`;
        let s = tokenFlows.get(key);
        if (!s) { s = new Set(); tokenFlows.set(key, s); }
        for (const f of feeders) {
          if (poolAddrs.has(f.from)) s.add(f.from);
        }
      }
    }

    for (let i = 0; i < splitSteps.length; i++) {
      const up = splitSteps[i];
      if (up.tokenOut === outputToken) continue;
      // Only chain steps created by per-transfer splitting, where each step
      // represents a distinct batch flowing to a specific downstream pool
      if (!up.perTransferSplit) continue;

      for (let j = 0; j < splitSteps.length; j++) {
        if (j === i || chainedDownstream.has(j)) continue;
        const down = splitSteps[j];
        if (down.tokenIn !== up.tokenOut) continue;
        if (down.tokenOut === up.tokenIn) continue;

        // Check if the upstream pool is the ONLY source of this intermediate to the downstream pool.
        // For V4 pools, the actual contract receiving tokens is V4_POOL_MANAGER, not the virtual pool address.
        const downPoolType = findPool(down.pool, down.tokenIn, down.tokenOut, poolMap)?.poolType;
        const downActualPool = (downPoolType === 9) ? V4_POOL_MANAGER : down.pool;
        const key = `${up.tokenOut}:${downActualPool}`;
        const sources = tokenFlows.get(key);
        if (!sources || sources.size !== 1 || !sources.has(up.pool)) continue;
        if (!down.perTransferSplit) {
          const ratio = down.amountIn === 0n ? 1n : (up.amountIn > down.amountIn ? up.amountIn / down.amountIn : down.amountIn / up.amountIn);
          if (ratio < 100n && up.amountIn > down.amountIn * 2n) continue;
        }

        chainMap.set(i, j);
        chainedDownstream.add(j);
        break;
      }
    }

    // Second pass: chain non-perTransferSplit steps that output an intermediate token
    // to downstream steps that convert it to the outputToken. This handles cases like
    // pharaoh(HERESY→WAVAX) + wombat(WAVAX→sAVAX) where both pharaoh steps need the
    // wombat step as a tail hop. Allow multiple upstream steps to share the same downstream.
    for (let i = 0; i < splitSteps.length; i++) {
      if (chainMap.has(i)) continue; // already chained
      const up = splitSteps[i];
      if (up.tokenOut === outputToken) continue;
      if (up.tokenIn === up.tokenOut) continue;
      // Find a downstream step (not yet chained as upstream) that converts this intermediate to outputToken
      for (let j = 0; j < splitSteps.length; j++) {
        if (j === i) continue;
        const down = splitSteps[j];
        if (down.tokenIn !== up.tokenOut) continue;
        if (down.tokenOut !== outputToken) continue;
        // This downstream step converts intermediate→outputToken, use it as tail hop
        chainMap.set(i, j);
        chainedDownstream.add(j);
        break;
      }
    }

    // Dedup pass A: suppress unchained steps that share the same pool + amountIn + tokenIn
    // as a chained upstream step (e.g., woofi_v2 USDC→WAVAX standalone alongside
    // woofi_v2 USDC→USDt chained to algebra USDt→WAVAX).
    for (let si = 0; si < splitSteps.length; si++) {
      if (chainedDownstream.has(si) || chainMap.has(si)) continue;
      const s = splitSteps[si];
      for (const [upIdx] of chainMap) {
        const up = splitSteps[upIdx];
        if (up.pool === s.pool && up.amountIn === s.amountIn && up.tokenIn === s.tokenIn) {
          chainedDownstream.add(si);
          break;
        }
      }
    }



    // Rebuild stepInputTokens excluding chained downstream steps
    stepInputTokens = new Set(
      splitSteps.filter((_, i) => !chainedDownstream.has(i)).map(s => s.tokenIn)
    );

    const steps: SwapStep[] = [];
    const routeParts: string[] = [];
    for (let si = 0; si < splitSteps.length; si++) {
      if (chainedDownstream.has(si)) continue; // consumed by an upstream chain

      const step = splitSteps[si];
      const f = hopToSwapFields(step, poolMap)!;

      // If this step is chained to a downstream step, build a multi-hop step
      if (chainMap.has(si)) {
        const downIdx = chainMap.get(si)!;
        const downStep = splitSteps[downIdx];
        const df = hopToSwapFields(downStep, poolMap)!;

        const allPools = [f.pool, df.pool];
        const allPoolTypes = [f.poolType, df.poolType];
        const allTokens = [step.tokenIn, step.tokenOut, downStep.tokenOut];
        const allExtraDatas = [f.extraData, df.extraData];
        const allProviders = [f.providerName, df.providerName];

        // If the chained result still doesn't reach the output token, extend with tail hops
        if (downStep.tokenOut !== outputToken && !stepInputTokens.has(downStep.tokenOut)) {
          let tailHops = findTailHops(downStep.tokenOut, outputToken, poolMap, traceLogs);

          // If downStep is a bridge-equivalent conversion (e.g. USDC→USDC.e),
          // also try finding a direct tail hop from the canonical token (e.g. USDC→USDt),
          // which skips the bridge hop entirely. Use the direct path if its best pool is
          // more recently active than the bridge path's best pool.
          const bridgeCanonical = BRIDGE_EQUIVALENTS[downStep.tokenOut];
          if (bridgeCanonical && bridgeCanonical === step.tokenOut) {
            const directTailHops = findTailHops(step.tokenOut, outputToken, poolMap, traceLogs);
            if (directTailHops.length > 0) {
              // Get latestSwapBlock for the bridge tail path's first hop
              const bridgeTailBlock = poolMap.get(tailHops[0]?.pool ?? "")?.latestSwapBlock ?? 0;
              // Get latestSwapBlock for the direct tail path — search poolMap for the
              // best matching pool (handles V4 virtual pools whose key differs from hop.pool)
              let directTailBlock = 0;
              for (const [addr, stored] of poolMap) {
                const toks = stored.tokens.map(t => t.toLowerCase());
                if (toks.includes(step.tokenOut) && toks.includes(outputToken)) {
                  if (stored.latestSwapBlock > directTailBlock) directTailBlock = stored.latestSwapBlock;
                }
              }
              if (directTailBlock > bridgeTailBlock) {
                // Direct path has a more recently active pool — skip the bridge hop
                allPools.splice(1);
                allPoolTypes.splice(1);
                allTokens.splice(2);
                allExtraDatas.splice(1);
                allProviders.splice(1);
                tailHops = directTailHops;
              }
            }
          }

          if (tailHops.length > 0) {
            allPools.push(...tailHops.map(h => h.pool));
            allPoolTypes.push(...tailHops.map(h => h.poolType));
            allTokens.push(...tailHops.map(h => h.tokenOut));
            allExtraDatas.push(...tailHops.map(h => h.extraData));
            allProviders.push(...tailHops.map(h => h.providerName));
          }
        }

        const routeStr = allProviders
          .map((pn, i) => `${pn}(${tok(allTokens[i])}→${tok(allTokens[i + 1])})`)
          .join("→");
        steps.push({
          amountIn: step.amountIn.toString(),
          pools: allPools,
          poolTypes: allPoolTypes,
          tokens: allTokens,
          extraDatas: allExtraDatas,
          route: routeStr,
        });
        routeParts.push(`[${step.amountIn}] ${routeStr}`);
        continue;
      }

      // For per-transfer-split steps that weren't chained: try to reuse a sibling's
      // downstream pool (a sibling = same upstream pool, same tokenIn/tokenOut, but chained).
      if (step.perTransferSplit && step.tokenOut !== outputToken && !stepInputTokens.has(step.tokenOut)) {
        let siblingFound = false;
        for (let sj = 0; sj < splitSteps.length; sj++) {
          if (!chainMap.has(sj)) continue;
          const sib = splitSteps[sj];
          if (sib.pool === step.pool && sib.tokenIn === step.tokenIn && sib.tokenOut === step.tokenOut) {
            // Reuse the sibling's chained downstream
            const downIdx = chainMap.get(sj)!;
            const downStep = splitSteps[downIdx];
            const df = hopToSwapFields(downStep, poolMap)!;

            const allPools = [f.pool, df.pool];
            const allPoolTypes = [f.poolType, df.poolType];
            const allTokens = [step.tokenIn, step.tokenOut, downStep.tokenOut];
            const allExtraDatas = [f.extraData, df.extraData];
            const allProviders = [f.providerName, df.providerName];

            // If the sibling chain still doesn't reach the output token, extend with tail hops
            if (downStep.tokenOut !== outputToken && !stepInputTokens.has(downStep.tokenOut)) {
              const tailHops = findTailHops(downStep.tokenOut, outputToken, poolMap, traceLogs);
              if (tailHops.length > 0) {
                allPools.push(...tailHops.map(h => h.pool));
                allPoolTypes.push(...tailHops.map(h => h.poolType));
                allTokens.push(...tailHops.map(h => h.tokenOut));
                allExtraDatas.push(...tailHops.map(h => h.extraData));
                allProviders.push(...tailHops.map(h => h.providerName));
              }
            }

            const routeStr = allProviders
              .map((pn, i) => `${pn}(${tok(allTokens[i])}→${tok(allTokens[i + 1])})`)
              .join("→");
            steps.push({
              amountIn: step.amountIn.toString(),
              pools: allPools,
              poolTypes: allPoolTypes,
              tokens: allTokens,
              extraDatas: allExtraDatas,
              route: routeStr,
            });
            routeParts.push(`[${step.amountIn}] ${routeStr}`);
            siblingFound = true;
            break;
          }
        }
        if (siblingFound) continue;
      }

      // If this step produces an intermediate token (not the final output token)
      // AND no other step consumes that intermediate, try to extend with tail hops.
      if (step.tokenOut !== outputToken && !stepInputTokens.has(step.tokenOut)) {
        const tailHops = findTailHops(step.tokenOut, outputToken, poolMap, traceLogs);
        if (tailHops.length > 0) {
          const allPools = [f.pool, ...tailHops.map(h => h.pool)];
          const allPoolTypes = [f.poolType, ...tailHops.map(h => h.poolType)];
          const allTokens = [step.tokenIn, step.tokenOut, ...tailHops.map(h => h.tokenOut)];
          const allExtraDatas = [f.extraData, ...tailHops.map(h => h.extraData)];
          const routeStr = [f.providerName, ...tailHops.map(h => h.providerName)]
            .map((pn, i) => `${pn}(${tok(allTokens[i])}→${tok(allTokens[i + 1])})`)
            .join("→");
          steps.push({
            amountIn: step.amountIn.toString(),
            pools: allPools,
            poolTypes: allPoolTypes,
            tokens: allTokens,
            extraDatas: allExtraDatas,
            route: routeStr,
          });
          routeParts.push(`[${step.amountIn}] ${routeStr}`);
          continue;
        }
      }

      steps.push({
        amountIn: step.amountIn.toString(),
        pools: [f.pool],
        poolTypes: [f.poolType],
        tokens: [step.tokenIn, step.tokenOut],
        extraDatas: [f.extraData],
        route: `${f.providerName}(${tok(step.tokenIn)}→${tok(step.tokenOut)})`,
      });
      routeParts.push(`[${step.amountIn}] ${f.providerName}(${tok(step.tokenIn)}→${tok(step.tokenOut)})`);
    }

    const producesOutput = steps.some(s => tokenMatchesOutput(s.tokens[s.tokens.length - 1], outputToken));
    if (!producesOutput) {
      base.unsupportedReason = `no step produces output token ${tok(outputToken)}`;
      base.route = `SPLIT(${steps.length}): ${routeParts.join(" → ")}`;
      return base;
    }

    base.supported = true;
    base.steps = steps;
    base.route = `SPLIT(${steps.length}): ${routeParts.join(" → ")}`;
    return base;
  }

  if (hops.length === 0) {
    base.unsupportedReason = "no swap events detected";
    return base;
  }

  const missingPools: string[] = [];
  for (const hop of hops) {
    if (!findPool(hop.pool, hop.tokenIn, hop.tokenOut, poolMap)) missingPools.push(hop.pool);
  }
  if (missingPools.length > 0) {
    base.unsupportedReason = `missing pools: ${missingPools.join(", ")}`;
    base.route = hops.map(h => `${h.pool} ${tok(h.tokenIn)}→${tok(h.tokenOut)}`).join(" → ");
    return base;
  }

  const pools: string[] = [];
  const poolTypes: number[] = [];
  const tokens: string[] = [hops[0].tokenIn];
  const extraDatas: string[] = [];
  const routeParts: string[] = [];

  for (const hop of hops) {
    // For Hashflow vaults (TRANSFER_FROM), extract the output amount from transfers
    let rfqOutputAmount: bigint | undefined;
    if (HASHFLOW_VAULTS.has(hop.pool)) {
      const vaultOutTransfers = transfers.filter(t => t.from === hop.pool && t.token === hop.tokenOut);
      rfqOutputAmount = vaultOutTransfers.reduce((sum, t) => sum + t.amount, 0n);
    }
    const f = hopToSwapFields({ ...hop, rfqOutputAmount }, poolMap)!;
    pools.push(f.pool);
    poolTypes.push(f.poolType);
    tokens.push(hop.tokenOut);
    extraDatas.push(f.extraData);
    routeParts.push(`${f.providerName}(${tok(hop.tokenIn)}→${tok(hop.tokenOut)})`);
  }

  // Validate that the route actually reaches the output token
  const lastToken = hops[hops.length - 1].tokenOut;
  if (lastToken !== outputToken) {
    // Try to extend the route with tail hops (e.g. USDt→USDC via V4, WAVAX→sAVAX via ERC4626)
    const tailHops = findTailHops(lastToken, outputToken, poolMap, traceLogs);
    if (tailHops.length > 0) {
      for (const th of tailHops) {
        pools.push(th.pool);
        poolTypes.push(th.poolType);
        tokens.push(th.tokenOut);
        extraDatas.push(th.extraData);
        routeParts.push(`${th.providerName}(${tok(tokens[tokens.length - 2])}→${tok(th.tokenOut)})`);
      }
    } else {
      base.unsupportedReason = `route ends at ${tok(lastToken)}, not ${tok(outputToken)} (missing pool in catalog)`;
      base.route = routeParts.join(" → ");
      return base;
    }
  }

  base.supported = true;
  base.pools = pools;
  base.poolTypes = poolTypes;
  base.tokens = tokens;
  base.extraDatas = extraDatas;
  base.route = routeParts.join(" → ");
  return base;
}

// --- Main ---

async function main() {
  const txsFile = process.argv[2] ?? path.join(import.meta.dirname!, "txs.txt");
  const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
  const poolsPath = path.resolve(import.meta.dirname!, "../../pools/data/pools.txt");
  const outDir = path.join(import.meta.dirname!, "payloads");

  const { pools: poolMap } = loadPools(poolsPath);
  // Inject ERC4626 vaults so BalV3 half-buffered swaps can chain to ERC4626 unwrap
  for (const v of ERC4626_VAULTS) {
    if (!poolMap.has(v.address.toLowerCase())) {
      poolMap.set(v.address.toLowerCase(), v as StoredPool);
    }
  }
  // Inject generated buffered edges (type 11) so findTailHops can find AUSD→USDC, etc.
  // These are synthetic entries: underlyingIn→underlyingOut via BalV3 pool + ERC4626 wrap/unwrap.
  for (const e of generateBufferedEdges(poolMap.values())) {
    // Key by pool+tokenIn+tokenOut so multiple directional edges don't collide
    const key = `${e.address}:${e.tokens[0]}:${e.tokens[1]}`;
    if (!poolMap.has(key)) {
      poolMap.set(key, e as StoredPool);
    }
  }
  // Inject trace-discovered UniV2-style pools not tracked by the pool discovery system.
  // These are pools from obscure DEX factories used in benchmark txs.
  const extraV2Pools: Array<{ address: string; tokens: string[] }> = [
    // USDC.e/WAVAX pool from AlligatorSwap factory 0xd9362aa8 — used in tx 0xc8b35844
    { address: "0x89810ddb66d08a532f51fb36e8fa108ebb36ca1a", tokens: ["0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664", "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7"] },
  ];
  for (const p of extraV2Pools) {
    const addr = p.address.toLowerCase();
    if (!poolMap.has(addr)) {
      poolMap.set(addr, { address: addr, providerName: "univ2_misc", poolType: 8 as any, tokens: p.tokens, latestSwapBlock: 80000000 } as StoredPool);
    }
  }

  console.log(`Loaded ${poolMap.size} pools from pools.txt`);

  const content = fs.readFileSync(txsFile, "utf-8");
  const hashes = content.split("\n")
    .map(line => line.replace(/#.*/, "").trim())
    .filter(line => line.startsWith("0x")) as Hex[];

  console.log(`Processing ${hashes.length} transactions...\n`);

  const wsUrl = process.env.WS_URL ?? "ws://localhost:9650/ext/bc/C/ws";
  const client = createPublicClient({
    chain: avalanche,
    transport: wsPool(wsUrl),
  });

  fs.mkdirSync(outDir, { recursive: true });

  for (const txHash of hashes) {
    // Skip if payload already exists
    const outPath = path.join(outDir, `${txHash}.json`);
    if (fs.existsSync(outPath)) {
      continue;
    }

    const [receipt, tx] = await Promise.all([
      client.getTransactionReceipt({ hash: txHash }),
      client.getTransaction({ hash: txHash }),
    ]);

    if (!receipt || receipt.status !== "success") {
      console.log(`${txHash.slice(0, 10)}: SKIP (reverted or missing)`);
      continue;
    }

    // Detect router from original receipt
    let router: RouterDef | undefined;
    let swapLog: Log | undefined;
    for (const r of ROUTERS) {
      swapLog = (receipt.logs as Log[]).find(
        l => l.topics[0] === r.swapTopic && l.address.toLowerCase() === r.address.toLowerCase()
      );
      if (swapLog) { router = r; break; }
    }

    if (!router || !swapLog) {
      console.log(`${txHash.slice(0, 10)}: SKIP (no recognized swap event)`);
      continue;
    }

    let { inputToken, outputToken, inputAmount } = router.parseSwapEvent(swapLog.data);
    if (inputToken === ZERO_ADDR) inputToken = WAVAX;
    if (outputToken === ZERO_ADDR) outputToken = WAVAX;

    // Skip txs that touch RFQ pools (check original receipt)
    const receiptTransfers = parseTransfers(receipt.logs as Log[]);
    const usesRfq = receiptTransfers.some(t => RFQ_POOLS.has(t.from) || RFQ_POOLS.has(t.to));
    if (usesRfq) {
      console.log(`SKIP ${txHash.slice(0, 10)} [${router.name}] ${tok(inputToken)}→${tok(outputToken)} — uses RFQ pool`);
      continue;
    }

    // Replay original tx at block-1 (start-of-block state)
    const parentBlock = `0x${(receipt.blockNumber - 1n).toString(16)}`;
    let trace: any;
    try {
      trace = await client.request({
        method: "debug_traceCall" as any,
        params: [
          {
            from: tx.from,
            to: tx.to,
            data: tx.input,
            value: tx.value ? `0x${tx.value.toString(16)}` : undefined,
            gas: "0x1C9C380", // 30M
          },
          parentBlock,
          {
            tracer: "callTracer",
            tracerConfig: { withLog: true },
          },
        ] as any,
      });
    } catch (err: any) {
      console.log(`SKIP ${txHash.slice(0, 10)} [${router.name}] ${tok(inputToken)}→${tok(outputToken)} — replay failed: ${err.message?.slice(0, 80)}`);
      continue;
    }

    if ((trace as any).error) {
      console.log(`SKIP ${txHash.slice(0, 10)} [${router.name}] ${tok(inputToken)}→${tok(outputToken)} — reverts at block start (backrun?)`);
      continue;
    }

    // Parse Transfer events from the replay trace
    const traceLogs = collectTraceLogs(trace);
    const transfers = parseTraceTransfers(traceLogs);

    // Calculate output amount from replay: sum of outputToken transfers to tx.from
    const txSender = tx.from.toLowerCase();
    let replayOutputAmount = transfers
      .filter(t => t.token === outputToken && t.to === txSender)
      .reduce((sum, t) => sum + t.amount, 0n);

    // When outputToken is WAVAX, the LFJ router unwraps it to native AVAX before
    // sending to the user. The WAVAX Transfer goes to the router, not the sender.
    if (replayOutputAmount === 0n && outputToken === WAVAX) {
      const routerAddr = router.address.toLowerCase();
      replayOutputAmount = transfers
        .filter(t => t.token === WAVAX && t.to === routerAddr)
        .reduce((sum, t) => sum + t.amount, 0n);
    }

    if (replayOutputAmount === 0n) {
      console.log(`SKIP ${txHash.slice(0, 10)} [${router.name}] ${tok(inputToken)}→${tok(outputToken)} — replay produced 0 output`);
      continue;
    }

    const excludeAddrs = new Set([
      router.address.toLowerCase(),
      txSender,
      ...ROUTERS.map(r => r.address.toLowerCase()),
    ]);

    let isSplit = detectSplit(transfers, excludeAddrs, poolMap);
    const v4PoolIdMap = buildV4PoolIdMap(poolMap);

    // V4-aware split detection: if no split detected yet but V4 swap events exist
    // alongside regular pool hops handling the same inputToken, it's a parallel split.
    // Example: pharaoh+DODO path for 499.65 WAVAX and V4 path for 0.35 WAVAX.
    if (!isSplit) {
      const v4StepsForSplit = extractV4TraceSteps(traceLogs, v4PoolIdMap);
      if (v4StepsForSplit.length > 0) {
        const normV4Tok = (t: string) => t === ZERO_ADDR ? WAVAX : t;
        const hopsForSplit = extractPoolHops(transfers, poolMap);
        // Check if V4 steps handle the same input token as regular hops — parallel split
        if (hopsForSplit.length > 0) {
          const hopInputTokens = new Set(hopsForSplit.map(h => h.tokenIn));
          const v4HandlesInput = v4StepsForSplit.some(s =>
            hopInputTokens.has(normV4Tok(s.tokenIn)) || normV4Tok(s.tokenIn) === inputToken
          );
          if (v4HandlesInput) isSplit = true;
        }
      }
    }
    const hops = extractPoolHops(transfers, poolMap);
    const wombatSteps = extractWombatTraceSteps(traceLogs, transfers, poolMap);
    const synapseSteps = extractSynapseTraceSteps(traceLogs, transfers, poolMap);
    wombatSteps.push(...synapseSteps);
    const balancerBufferedSteps = extractBalancerBufferedTraceSteps(traceLogs, transfers, poolMap);

    // For single routes: merge Wombat/Platypus hops into hops list
    if (!isSplit && hops.length === 0 && wombatSteps.length > 0) {
      for (const ws of wombatSteps) {
        hops.push({ pool: ws.pool, tokenIn: ws.tokenIn, tokenOut: ws.tokenOut });
      }
    }

    // For single routes: merge Balancer V3 buffered hops (same pattern as Wombat above)
    if (!isSplit && hops.length === 0 && balancerBufferedSteps.length > 0) {
      for (const bs of balancerBufferedSteps) {
        hops.push({ pool: bs.pool, tokenIn: bs.tokenIn, tokenOut: bs.tokenOut,
          bufWrappedIn: bs.bufWrappedIn, bufPool: bs.bufPool, bufWrappedOut: bs.bufWrappedOut });
      }
    }

    // If single-path and the first hop's tokenIn doesn't match the swap event's inputToken,
    // check if a Wombat/Platypus step bridges the gap (e.g. USDC→USDt.e before lfj_v1).
    if (!isSplit && hops.length > 0 && hops[0].tokenIn !== inputToken && wombatSteps.length > 0) {
      const bridgeStep = wombatSteps.find(s => s.tokenIn === inputToken && s.tokenOut === hops[0].tokenIn);
      if (bridgeStep) {
        hops.unshift({ pool: bridgeStep.pool, tokenIn: bridgeStep.tokenIn, tokenOut: bridgeStep.tokenOut });
      }
    }

    // Wombat/Platypus tail gap: last hop doesn't reach outputToken
    if (!isSplit && hops.length > 0 && hops[hops.length - 1].tokenOut !== outputToken && wombatSteps.length > 0) {
      const tailStep = wombatSteps.find(s => s.tokenIn === hops[hops.length - 1].tokenOut && s.tokenOut === outputToken);
      if (tailStep) {
        hops.push({ pool: tailStep.pool, tokenIn: tailStep.tokenIn, tokenOut: tailStep.tokenOut });
      }
    }

    // Wombat/Platypus inter-hop gap filling: bridge gaps between adjacent hops
    if (!isSplit && hops.length > 1 && wombatSteps.length > 0) {
      for (let idx = 0; idx < hops.length - 1; idx++) {
        if (hops[idx].tokenOut !== hops[idx + 1].tokenIn) {
          const gapStep = wombatSteps.find(s => s.tokenIn === hops[idx].tokenOut && s.tokenOut === hops[idx + 1].tokenIn);
          if (gapStep) {
            hops.splice(idx + 1, 0, { pool: gapStep.pool, tokenIn: gapStep.tokenIn, tokenOut: gapStep.tokenOut });
          }
        }
      }
    }

    // Balancer V3 buffered head bridge: first hop doesn't start at inputToken
    if (!isSplit && hops.length > 0 && hops[0].tokenIn !== inputToken && balancerBufferedSteps.length > 0) {
      const bridgeStep = balancerBufferedSteps.find(s => s.tokenIn === inputToken && s.tokenOut === hops[0].tokenIn);
      if (bridgeStep) {
        hops.unshift({ pool: bridgeStep.pool, tokenIn: bridgeStep.tokenIn, tokenOut: bridgeStep.tokenOut,
          bufWrappedIn: bridgeStep.bufWrappedIn, bufPool: bridgeStep.bufPool, bufWrappedOut: bridgeStep.bufWrappedOut });
      }
    }

    // Balancer V3 buffered tail gap: last hop doesn't reach outputToken
    if (!isSplit && hops.length > 0 && hops[hops.length - 1].tokenOut !== outputToken && balancerBufferedSteps.length > 0) {
      const tailStep = balancerBufferedSteps.find(s => s.tokenIn === hops[hops.length - 1].tokenOut && s.tokenOut === outputToken);
      if (tailStep) {
        hops.push({ pool: tailStep.pool, tokenIn: tailStep.tokenIn, tokenOut: tailStep.tokenOut,
          bufWrappedIn: tailStep.bufWrappedIn, bufPool: tailStep.bufPool, bufWrappedOut: tailStep.bufWrappedOut });
      }
    }

    // Balancer V3 buffered inter-hop gap filling
    if (!isSplit && hops.length > 1 && balancerBufferedSteps.length > 0) {
      for (let idx = 0; idx < hops.length - 1; idx++) {
        if (hops[idx].tokenOut !== hops[idx + 1].tokenIn) {
          const gapStep = balancerBufferedSteps.find(s => s.tokenIn === hops[idx].tokenOut && s.tokenOut === hops[idx + 1].tokenIn);
          if (gapStep) {
            hops.splice(idx + 1, 0, { pool: gapStep.pool, tokenIn: gapStep.tokenIn, tokenOut: gapStep.tokenOut,
              bufWrappedIn: gapStep.bufWrappedIn, bufPool: gapStep.bufPool, bufWrappedOut: gapStep.bufWrappedOut });
          }
        }
      }
    }

    // V4 single-hop fallback: when no standard pool hops found and not a split,
    // check for V4 Swap events (e.g. USDt→USDC through PoolManager singleton).
    // Route these through the split path which already handles V4 steps.
    // V4 integration for single-path routes: extractPoolHops misses V4 because the
    // PoolManager is a singleton not in poolMap. Detect V4 Swap events from the trace
    // and merge them into hops (either as standalone or as part of a multi-hop route).
    let v4SingleHop = false;
    const v4Steps = (!isSplit) ? extractV4TraceSteps(traceLogs, v4PoolIdMap) : [];
    if (!isSplit && v4Steps.length > 0) {
      if (hops.length === 0) {
        // Pure V4 single-hop: route through split path
        v4SingleHop = true;
      } else {
        // Multi-hop with V4: merge V4 steps that bridge gaps in the route chain
        // V4 pools use native AVAX (ZERO_ADDR) but routes use WAVAX — normalize for matching
        const normV4 = (t: string) => t === ZERO_ADDR ? WAVAX : t;
        const makeV4Hop = (s: { pool: string; tokenIn: string; tokenOut: string }) => {
          const needsWrap = s.tokenIn === ZERO_ADDR || s.tokenOut === ZERO_ADDR;
          const stored = poolMap.get(s.pool);
          const hop: any = { pool: s.pool, tokenIn: normV4(s.tokenIn), tokenOut: normV4(s.tokenOut) };
          if (needsWrap && stored) hop._wrapNativeExtraData = stored.extraData + ",wrapNative=1";
          return hop;
        };
        const lastHop = hops[hops.length - 1];
        if (lastHop.tokenOut !== outputToken) {
          const tailV4 = v4Steps.find(s => normV4(s.tokenIn) === lastHop.tokenOut && normV4(s.tokenOut) === outputToken);
          if (tailV4) {
            hops.push(makeV4Hop(tailV4));
          } else {
            // Try 2-hop V4 chain: lastHop.tokenOut → mid → outputToken
            for (const s1 of v4Steps) {
              if (normV4(s1.tokenIn) !== lastHop.tokenOut) continue;
              const mid = normV4(s1.tokenOut);
              if (mid === lastHop.tokenOut || mid === outputToken) continue;
              const s2 = v4Steps.find(s => normV4(s.tokenIn) === mid && normV4(s.tokenOut) === outputToken && s.logIndex > s1.logIndex);
              if (s2) {
                hops.push(makeV4Hop(s1));
                hops.push(makeV4Hop(s2));
                break;
              }
            }
          }
        }
        const firstHop = hops[0];
        if (firstHop.tokenIn !== inputToken) {
          const headV4 = v4Steps.find(s => normV4(s.tokenIn) === inputToken && normV4(s.tokenOut) === firstHop.tokenIn);
          if (headV4) {
            hops.unshift(makeV4Hop(headV4));
          }
        }
        // Fill gaps between adjacent hops
        for (let idx = 0; idx < hops.length - 1; idx++) {
          if (hops[idx].tokenOut !== hops[idx + 1].tokenIn) {
            const gapV4 = v4Steps.find(s => normV4(s.tokenIn) === hops[idx].tokenOut && normV4(s.tokenOut) === hops[idx + 1].tokenIn);
            if (gapV4) {
              hops.splice(idx + 1, 0, makeV4Hop(gapV4));
            }
          }
        }
      }
    }

    // Generic single-path head bridge: if the reconstructed route still starts at the
    // wrong token, prepend the traced/known hop(s) that convert the swap input into
    // the first detected hop's input. This fixes cases like BTC.b→WBTC.e pre-hops.
    if (!isSplit && !v4SingleHop && hops.length > 0 && hops[0].tokenIn !== inputToken) {
      const headHops = findTailHops(inputToken, hops[0].tokenIn, poolMap, traceLogs);
      if (headHops.length > 0) {
        const prepend: PoolHop[] = [];
        let currentToken = inputToken;
        for (const hh of headHops) {
          prepend.push({ pool: hh.pool, tokenIn: currentToken, tokenOut: hh.tokenOut });
          currentToken = hh.tokenOut;
        }
        if (currentToken === hops[0].tokenIn) {
          hops.unshift(...prepend);
        }
      } else {
        // Fallback: findTailHops couldn't find a bridge pool. Search trace Transfer
        // events for a pool that received the inputToken and produced the first hop's
        // tokenIn (e.g. LZ proxy tokens like Frax, WBTC.OFT).
        const headTarget = hops[0].tokenIn;
        const candidatePools = new Map<string, { hasIn: boolean; hasOut: boolean }>();
        for (const t of transfers) {
          if (t.from === ZERO_ADDR || t.to === ZERO_ADDR) continue;
          if (t.token === inputToken && t.to !== txSender && t.to !== router!.address.toLowerCase()) {
            const entry = candidatePools.get(t.to) ?? { hasIn: false, hasOut: false };
            entry.hasIn = true;
            candidatePools.set(t.to, entry);
          }
          if (t.token === headTarget && t.from !== txSender && t.from !== router!.address.toLowerCase()) {
            const entry = candidatePools.get(t.from) ?? { hasIn: false, hasOut: false };
            entry.hasOut = true;
            candidatePools.set(t.from, entry);
          }
        }
        for (const [addr, { hasIn, hasOut }] of candidatePools) {
          if (!hasIn || !hasOut) continue;
          const stored = findPool(addr, inputToken, headTarget, poolMap);
          if (stored) {
            hops.unshift({ pool: addr, tokenIn: inputToken, tokenOut: headTarget });
            break;
          }
        }
      }

      // If head bridge still not found, mark as unsupported (LZ proxy, cross-chain bridge, etc.)
      if (hops.length > 0 && hops[0].tokenIn !== inputToken) {
        const payload = { block: Number(receipt.blockNumber), txHash, source: router.name, isSplit: false, inputToken, outputToken, amountIn: String(inputAmount), expectedOut: String(replayOutputAmount), gasUsed: Number(receipt.gasUsed), supported: false, unsupportedReason: `missing head bridge: ${inputToken.slice(0,10)}→${hops[0].tokenIn.slice(0,10)}` };
        fs.writeFileSync(path.join(outDir, `${txHash}.json`), JSON.stringify(payload, null, 2) + "\n");
        console.log(`✗ ${txHash.slice(0,10)} [${router.name}] ${tok(inputToken)}→${tok(outputToken)} — missing head bridge`);
        continue;
      }
    }

    // Diamond/parallel route detection: after assembling the single-path hop chain,
    // check if the chain is actually invalid (broken chaining where hop[i].tokenIn
    // !== hop[i-1].tokenOut). This happens when a diamond pattern exists: the input
    // splits into parallel paths that merge to the same output. When detected,
    // convert from single route to split route.
    if (!isSplit && !v4SingleHop && hops.length > 1) {
      let hasBrokenChain = false;
      for (let i = 1; i < hops.length; i++) {
        if (hops[i].tokenIn !== hops[i - 1].tokenOut) {
          hasBrokenChain = true;
          break;
        }
      }
      // Also detect diamond when the primary inputToken is used as input by multiple hops
      if (!hasBrokenChain) {
        const inputHopCount = hops.filter(h => h.tokenIn === inputToken).length;
        if (inputHopCount >= 2) hasBrokenChain = true;
      }
      if (hasBrokenChain) {
        // Convert to split route: group hops into paths where each path is a chain
        // of consecutive hops that connect (tokenOut→tokenIn).
        isSplit = true;
      }
    }

    const splitSteps = (isSplit || v4SingleHop) ? extractSplitSteps(transfers, poolMap, traceLogs, v4PoolIdMap) : [];
    // Merge Wombat and Balancer buffered steps into split steps (avoiding duplicates)
    if (isSplit || v4SingleHop) {
      const splitPoolTokenSet = new Set(splitSteps.map(s => `${s.pool}:${s.tokenIn}:${s.tokenOut}`));
      for (const ws of [...wombatSteps, ...balancerBufferedSteps]) {
        // Include bufPool in key for Balancer V3 buffered steps: the same vault can
        // handle multiple swaps with the same token pair via different internal pools.
        const key = ws.bufPool
          ? `${ws.pool}:${ws.tokenIn}:${ws.tokenOut}:${ws.bufPool}`
          : `${ws.pool}:${ws.tokenIn}:${ws.tokenOut}`;
        if (!splitPoolTokenSet.has(key)) {
          splitSteps.push(ws);
          splitPoolTokenSet.add(key);
        }
      }
      splitSteps.sort((a, b) => a.logIndex - b.logIndex);
    }

    const payload = buildPayload(
      txHash,
      Number(receipt.blockNumber),
      router.name,
      isSplit || v4SingleHop,
      inputToken,
      outputToken,
      inputAmount,
      replayOutputAmount,
      receipt.gasUsed,
      hops,
      poolMap,
      splitSteps,
      traceLogs,
      transfers,
    );

    const outFile = path.join(outDir, `${txHash}.json`);
    fs.writeFileSync(outFile, JSON.stringify(payload, null, 2) + "\n");

    const tag = payload.supported ? "✓" : "✗";
    const reason = payload.supported ? payload.route : payload.unsupportedReason;
    console.log(`${tag} ${txHash.slice(0, 10)} [${router.name}] ${tok(inputToken)}→${tok(outputToken)} ${payload.isSplit ? "SPLIT" : "SINGLE"} — ${reason}`);
  }
}

main().catch(console.error);
