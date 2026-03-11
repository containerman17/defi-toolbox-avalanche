import { encodeFunctionData, encodeAbiParameters, type Hex } from "viem";
import { type StoredPool, POOL_TYPE_UNIV4, POOL_TYPE_BALANCER_V3_BUFFERED, POOL_TYPE_TRANSFER_FROM, POOL_TYPE_V2, POOL_TYPE_BALANCER_V2, POOL_TYPE_CAVALRE, POOL_TYPE_KYBER_DMM, POOL_TYPE_SYNAPSE, POOL_TYPE_TRIDENT } from "../pools/index.ts";

const V4_POOL_MANAGER = "0x06380C0e0912312B5150364B9DC4542BA0DbBc85";

export interface RouteStep {
  pool: StoredPool;
  tokenIn: string;
  tokenOut: string;
}

export interface FlatStep {
  pool: StoredPool;
  tokenIn: string;
  tokenOut: string;
  amountIn: bigint; // 0 = use contract's balance of tokenIn
}

const swapAbi = [
  {
    name: "swap",
    type: "function",
    inputs: [
      { name: "pools", type: "address[]" },
      { name: "poolTypes", type: "uint8[]" },
      { name: "tokens", type: "address[]" },
      { name: "amountsIn", type: "uint256[]" },
      { name: "extraDatas", type: "bytes[]" },
    ],
    outputs: [{ type: "uint256" }],
  },
] as const;

/**
 * Parse V4 extraData string "id=0x...,fee=500,ts=10,hooks=0x..." into on-chain bytes.
 * On-chain format: abi.encode(uint256 fee, int256 tickSpacing, address hooks)
 */
/**
 * Parse Balancer V3 Buffered extraData.
 * Accepts either:
 * - Key-value format: "wi=0x...,bp=0x...,wo=0x..."
 * - Pre-encoded ABI hex: "0x000..."
 * On-chain format: abi.encode(address wrappedIn, address pool, address wrappedOut)
 */
function encodeBufferedExtraData(extraData: string): Hex {
  // If already ABI-encoded (starts with 0x and is long enough)
  if (extraData.startsWith("0x") && extraData.length >= 194) {
    return extraData as Hex;
  }
  // Key-value format
  const parts: Record<string, string> = {};
  for (const kv of extraData.split(",")) {
    const eq = kv.indexOf("=");
    if (eq > 0) parts[kv.slice(0, eq)] = kv.slice(eq + 1);
  }
  return encodeAbiParameters(
    [{ type: "address" }, { type: "address" }, { type: "address" }],
    [parts.wi as Hex, parts.bp as Hex, parts.wo as Hex],
  );
}

function encodeV4ExtraData(extraData: string): Hex {
  const parts: Record<string, string> = {};
  for (const kv of extraData.split(",")) {
    const eq = kv.indexOf("=");
    if (eq > 0) parts[kv.slice(0, eq)] = kv.slice(eq + 1);
  }

  const fee = BigInt(parts.fee);
  const tickSpacing = BigInt(parts.ts);
  const hooks = parts.hooks as Hex;
  // wrapNative=1 means the V4 pool uses native AVAX but the step uses WAVAX:
  // the router should unwrap WAVAX→AVAX before the swap and wrap AVAX→WAVAX after.
  const wrapNative = BigInt(parts.wrapNative ?? "0");

  return encodeAbiParameters(
    [{ type: "uint256" }, { type: "int256" }, { type: "address" }, { type: "uint256" }],
    [fee, tickSpacing, hooks, wrapNative],
  );
}

/**
 * Encode a multi-hop swap route into calldata for the Hayabusa router contract.
 *
 * For V4 pools: uses the PoolManager address instead of the pseudo-address,
 * and encodes fee/tickSpacing/hooks into the extraData bytes.
 * For all others: uses the stored pool address and empty extraData.
 */
/**
 * Encode a single step's pool address and extraData based on pool type.
 */
function encodeStepPoolAndExtra(step: { pool: StoredPool }): { pool: Hex; extraData: Hex } {
  if (step.pool.poolType === POOL_TYPE_UNIV4) {
    return { pool: V4_POOL_MANAGER as Hex, extraData: encodeV4ExtraData(step.pool.extraData!) };
  } else if (step.pool.poolType === POOL_TYPE_BALANCER_V3_BUFFERED) {
    return { pool: step.pool.address as Hex, extraData: encodeBufferedExtraData(step.pool.extraData!) };
  } else if (step.pool.poolType === POOL_TYPE_BALANCER_V2) {
    const poolId = step.pool.extraData!.replace("poolId=", "") as Hex;
    return { pool: step.pool.address as Hex, extraData: encodeAbiParameters([{ type: "bytes32" }], [poolId]) };
  } else if (step.pool.poolType === POOL_TYPE_TRANSFER_FROM) {
    return { pool: step.pool.address as Hex, extraData: step.pool.extraData as Hex ?? "0x" };
  } else if (step.pool.poolType === POOL_TYPE_CAVALRE) {
    return { pool: step.pool.address as Hex, extraData: encodeAbiParameters([{ type: "address" }], [step.pool.address as Hex]) };
  } else if (step.pool.poolType === POOL_TYPE_SYNAPSE) {
    const parts: Record<string, string> = {};
    for (const kv of (step.pool.extraData ?? "").split(",")) {
      const eq = kv.indexOf("=");
      if (eq > 0) parts[kv.slice(0, eq)] = kv.slice(eq + 1);
    }
    return { pool: step.pool.address as Hex, extraData: encodeAbiParameters([{ type: "uint8" }, { type: "uint8" }], [Number(parts.from), Number(parts.to)]) };
  } else if (step.pool.poolType === POOL_TYPE_TRIDENT) {
    const parts: Record<string, string> = {};
    for (const kv of (step.pool.extraData ?? "").split(",")) {
      const eq = kv.indexOf("=");
      if (eq > 0) parts[kv.slice(0, eq)] = kv.slice(eq + 1);
    }
    return { pool: step.pool.address as Hex, extraData: encodeAbiParameters([{ type: "address" }], [parts.bento as Hex]) };
  } else if (step.pool.poolType === POOL_TYPE_V2 && step.pool.extraData && step.pool.extraData.startsWith("fee=")) {
    const feeBps = BigInt(step.pool.extraData.slice(4));
    return { pool: step.pool.address as Hex, extraData: encodeAbiParameters([{ type: "uint256" }], [feeBps]) };
  } else {
    return { pool: step.pool.address as Hex, extraData: "0x" };
  }
}

/**
 * Encode a flat list of steps into calldata for the Hayabusa router's swap() function.
 */
export function encodeSwapFlat(steps: FlatStep[]): Hex {
  const pools: Hex[] = [];
  const poolTypes: number[] = [];
  const tokens: Hex[] = []; // interleaved: [tokenIn0, tokenOut0, tokenIn1, tokenOut1, ...]
  const amountsIn: bigint[] = [];
  const extraDatas: Hex[] = [];

  for (const step of steps) {
    const { pool, extraData } = encodeStepPoolAndExtra(step);
    pools.push(pool);
    poolTypes.push(step.pool.poolType);
    tokens.push(step.tokenIn as Hex);
    tokens.push(step.tokenOut as Hex);
    amountsIn.push(step.amountIn);
    extraDatas.push(extraData);
  }

  return encodeFunctionData({
    abi: swapAbi,
    functionName: "swap",
    args: [pools, poolTypes, tokens, amountsIn, extraDatas],
  });
}

/**
 * Encode a multi-hop swap route into calldata for the Hayabusa router contract.
 * Converts the route to flat format and delegates to encodeSwapFlat.
 */
export function encodeSwap(route: RouteStep[], amountIn: bigint): Hex {
  const steps: FlatStep[] = route.map((step, i) => ({
    ...step,
    amountIn: i === 0 ? amountIn : 0n,
  }));
  return encodeSwapFlat(steps);
}
