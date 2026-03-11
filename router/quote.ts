import { readFileSync } from "node:fs";
import { join } from "node:path";
import { type PublicClient, type Hex, type Transport, type Chain, decodeAbiParameters } from "viem";
import { encodeSwap, encodeSwapFlat, type RouteStep, type FlatStep } from "./encode.ts";
import { getBalanceOverride, getAllowanceOverride, getBalanceOverrideAsync, getHookOverrides, isReflectionToken } from "./overrides.ts";
export const ROUTER_ADDRESS = "0x2bef1becdafcfe8990a233d03a98bbb39021c96e" as const;

const DUMMY_SENDER = "0x000000000000000000000000000000000000dEaD";
const NATIVE_TOKEN = "0x0000000000000000000000000000000000000000";

// Load compiled bytecode (includes V4 support not yet deployed on-chain)
let routerCodeCache: Hex | undefined;
function getRouterBytecode(): Hex {
  if (!routerCodeCache) {
    const hex = readFileSync(join(import.meta.dirname!, "contracts", "bytecode.hex"), "utf-8").trim();
    routerCodeCache = `0x${hex}` as Hex;
  }
  return routerCodeCache;
}

export async function quoteRoute(
  client: PublicClient,
  route: RouteStep[],
  amountIn: bigint,
  blockNumber?: bigint,
  extraStateOverrides?: Record<string, any>,
): Promise<bigint> {
  if (route.length === 0) throw new Error("empty route");

  const calldata = encodeSwap(route, amountIn);
  const inputToken = route[0].tokenIn.toLowerCase();
  const blockHex = blockNumber ? `0x${blockNumber.toString(16)}` : "latest";

  // Collect all tokens in the route for hook overrides
  const allTokens = new Set<string>();
  for (const step of route) {
    allTokens.add(step.tokenIn.toLowerCase());
    allTokens.add(step.tokenOut.toLowerCase());
  }

  // Build extra overrides for reflection tokens and hook contracts
  const mergedExtra: Record<string, any> = { ...(extraStateOverrides ?? {}) };
  if (inputToken !== NATIVE_TOKEN && isReflectionToken(inputToken)) {
    const reflOvr = await getBalanceOverrideAsync(client, inputToken, amountIn, ROUTER_ADDRESS, blockNumber);
    for (const [addr, val] of Object.entries(reflOvr)) {
      if (!mergedExtra[addr]) mergedExtra[addr] = { stateDiff: {} };
      if (!mergedExtra[addr].stateDiff) mergedExtra[addr].stateDiff = {};
      Object.assign(mergedExtra[addr].stateDiff, val.stateDiff);
    }
  }
  for (const token of allTokens) {
    const hookOvr = getHookOverrides(token);
    for (const [addr, val] of Object.entries(hookOvr)) {
      mergedExtra[addr] = { ...(mergedExtra[addr] ?? {}), ...val };
    }
  }

  const hasExtra = Object.keys(mergedExtra).length > 0;
  const stateOverride = buildStateOverrides(inputToken, amountIn, hasExtra ? mergedExtra : undefined);

  const result = await client.request({
    method: "eth_call" as any,
    params: [
      {
        from: DUMMY_SENDER,
        to: ROUTER_ADDRESS,
        data: calldata,
        value: inputToken === NATIVE_TOKEN ? `0x${amountIn.toString(16)}` : undefined,
      },
      blockHex,
      stateOverride,
    ] as any,
  });

  if (!result || result === "0x") {
    throw new Error("quoteRoute: empty response from eth_call");
  }

  const [amountOut] = decodeAbiParameters([{ type: "uint256" }], result as Hex);
  return amountOut;
}

export async function quoteFlat(
  client: PublicClient,
  steps: FlatStep[],
  inputToken: string,
  totalAmountIn: bigint,
  blockNumber?: bigint,
  extraStateOverrides?: Record<string, any>,
): Promise<bigint> {
  if (steps.length === 0) throw new Error("empty steps");

  const calldata = encodeSwapFlat(steps);
  const blockHex = blockNumber ? `0x${blockNumber.toString(16)}` : "latest";

  // Aggregate required balances per input token from all steps with explicit amountIn > 0
  const tokenBalances = new Map<string, bigint>();
  for (const step of steps) {
    if (step.amountIn > 0n) {
      const token = step.tokenIn.toLowerCase();
      tokenBalances.set(token, (tokenBalances.get(token) ?? 0n) + step.amountIn);
    }
  }
  // Ensure the primary input token is included
  const normalizedInput = inputToken.toLowerCase();
  if (!tokenBalances.has(normalizedInput) && totalAmountIn > 0n) {
    tokenBalances.set(normalizedInput, totalAmountIn);
  }

  // Collect all tokens for hook overrides
  const allTokens = new Set<string>();
  for (const step of steps) {
    allTokens.add(step.tokenIn.toLowerCase());
    allTokens.add(step.tokenOut.toLowerCase());
  }

  // Build extra overrides for reflection tokens and hook contracts
  const mergedExtra: Record<string, any> = { ...(extraStateOverrides ?? {}) };
  for (const [token, amount] of tokenBalances) {
    if (token !== NATIVE_TOKEN && isReflectionToken(token)) {
      const reflOvr = await getBalanceOverrideAsync(client, token, amount, ROUTER_ADDRESS, blockNumber);
      for (const [addr, val] of Object.entries(reflOvr)) {
        if (!mergedExtra[addr]) mergedExtra[addr] = { stateDiff: {} };
        if (!mergedExtra[addr].stateDiff) mergedExtra[addr].stateDiff = {};
        Object.assign(mergedExtra[addr].stateDiff, val.stateDiff);
      }
    }
  }
  for (const token of allTokens) {
    const hookOvr = getHookOverrides(token);
    for (const [addr, val] of Object.entries(hookOvr)) {
      mergedExtra[addr] = { ...(mergedExtra[addr] ?? {}), ...val };
    }
  }

  const hasExtra = Object.keys(mergedExtra).length > 0;
  // Build state overrides for all input tokens
  const stateOverride = buildFlatStateOverrides(tokenBalances, hasExtra ? mergedExtra : undefined);

  const result = await client.request({
    method: "eth_call" as any,
    params: [
      {
        from: DUMMY_SENDER,
        to: ROUTER_ADDRESS,
        data: calldata,
        value: normalizedInput === NATIVE_TOKEN ? `0x${totalAmountIn.toString(16)}` : undefined,
      },
      blockHex,
      stateOverride,
    ] as any,
  });

  if (!result || result === "0x") {
    throw new Error("quoteFlat: empty response from eth_call");
  }

  const [amountOut] = decodeAbiParameters([{ type: "uint256" }], result as Hex);
  return amountOut;
}

/**
 * Build state overrides for flat quoting with multiple input tokens.
 */
function buildFlatStateOverrides(tokenBalances: Map<string, bigint>, extraStateOverrides?: Record<string, any>) {
  const merged: Record<string, Record<string, Hex>> = {};

  for (const [token, amount] of tokenBalances) {
    if (token === NATIVE_TOKEN) continue;
    const balOvr = getBalanceOverride(token, amount, ROUTER_ADDRESS);
    for (const [addr, val] of Object.entries(balOvr)) {
      if (!merged[addr]) merged[addr] = {};
      Object.assign(merged[addr], val.stateDiff);
    }
  }

  const stateOverride: Record<string, any> = {};
  for (const [address, slots] of Object.entries(merged)) {
    stateOverride[address] = { stateDiff: slots };
  }

  if (tokenBalances.has(NATIVE_TOKEN)) {
    const nativeAmount = tokenBalances.get(NATIVE_TOKEN)!;
    stateOverride[DUMMY_SENDER] = {
      ...(stateOverride[DUMMY_SENDER] ?? {}),
      balance: `0x${nativeAmount.toString(16)}`,
    };
  }

  stateOverride[ROUTER_ADDRESS] = {
    ...(stateOverride[ROUTER_ADDRESS] ?? {}),
    code: getRouterBytecode(),
  };

  // Merge extra state overrides (e.g. for TRANSFER_FROM RFQ vaults)
  if (extraStateOverrides) {
    for (const [addr, ovr] of Object.entries(extraStateOverrides)) {
      if (stateOverride[addr]) {
        if (ovr.stateDiff && stateOverride[addr].stateDiff) {
          Object.assign(stateOverride[addr].stateDiff, ovr.stateDiff);
        } else if (ovr.stateDiff) {
          stateOverride[addr].stateDiff = ovr.stateDiff;
        }
      } else {
        stateOverride[addr] = ovr;
      }
    }
  }

  return stateOverride;
}

/**
 * Build geth-style state overrides for raw JSON-RPC calls.
 */
function buildStateOverrides(inputToken: string, amountIn: bigint, extraStateOverrides?: Record<string, any>) {
  // Set balance on the ROUTER (not the sender) since quoteRoute skips transferFrom.
  // This avoids double fee-on-transfer penalties.
  const balanceOverride = inputToken === NATIVE_TOKEN
    ? {}
    : getBalanceOverride(inputToken, amountIn, ROUTER_ADDRESS);
  const allowanceOverride = inputToken === NATIVE_TOKEN
    ? {}
    : {}; // No allowance needed — quoteRoute doesn't transferFrom

  const merged: Record<string, Record<string, Hex>> = {};
  for (const ovr of [balanceOverride, allowanceOverride]) {
    for (const [addr, val] of Object.entries(ovr)) {
      if (!merged[addr]) merged[addr] = {};
      Object.assign(merged[addr], val.stateDiff);
    }
  }

  const stateOverride: Record<string, any> = {};
  for (const [address, slots] of Object.entries(merged)) {
    stateOverride[address] = { stateDiff: slots };
  }

  if (inputToken === NATIVE_TOKEN) {
    stateOverride[DUMMY_SENDER] = {
      ...(stateOverride[DUMMY_SENDER] ?? {}),
      balance: `0x${amountIn.toString(16)}`,
    };
  }

  stateOverride[ROUTER_ADDRESS] = {
    ...(stateOverride[ROUTER_ADDRESS] ?? {}),
    code: getRouterBytecode(),
  };

  // Merge extra state overrides (e.g. for TRANSFER_FROM RFQ vaults)
  if (extraStateOverrides) {
    for (const [addr, ovr] of Object.entries(extraStateOverrides)) {
      if (stateOverride[addr]) {
        if (ovr.stateDiff && stateOverride[addr].stateDiff) {
          Object.assign(stateOverride[addr].stateDiff, ovr.stateDiff);
        } else if (ovr.stateDiff) {
          stateOverride[addr].stateDiff = ovr.stateDiff;
        }
      } else {
        stateOverride[addr] = ovr;
      }
    }
  }

  return stateOverride;
}

/**
 * Estimate gas for a swap via eth_estimateGas with state overrides.
 * Uses raw JSON-RPC since viem's estimateGas may not pass stateOverride on all versions.
 */
export async function estimateRouteGas(
  client: PublicClient,
  route: RouteStep[],
  amountIn: bigint,
  blockNumber?: bigint,
): Promise<bigint> {
  if (route.length === 0) throw new Error("empty route");

  const calldata = encodeSwap(route, amountIn);
  const inputToken = route[0].tokenIn.toLowerCase();
  const stateOverride = buildStateOverrides(inputToken, amountIn);
  const blockHex = blockNumber ? `0x${blockNumber.toString(16)}` : "latest";

  const resp = await client.request({
    method: "eth_estimateGas" as any,
    params: [
      {
        from: DUMMY_SENDER,
        to: ROUTER_ADDRESS,
        data: calldata,
        value: inputToken === NATIVE_TOKEN ? `0x${amountIn.toString(16)}` : undefined,
      },
      blockHex,
      stateOverride,
    ] as any,
  });

  return BigInt(resp as string);
}

/**
 * Get exact gas used for a swap via debug_traceCall with state overrides.
 * Unlike eth_estimateGas (which pads for 63/64 rule), debug_traceCall returns
 * the real gas consumed — matching revm's gas_used exactly.
 * Requires a node that supports debug_traceCall (e.g. local avalanchego).
 */
export async function traceRouteGas(
  client: PublicClient,
  route: RouteStep[],
  amountIn: bigint,
  blockNumber?: bigint,
): Promise<{ gas: bigint; failed: boolean }> {
  if (route.length === 0) throw new Error("empty route");

  const calldata = encodeSwap(route, amountIn);
  const inputToken = route[0].tokenIn.toLowerCase();
  const stateOverrides = buildStateOverrides(inputToken, amountIn);
  const blockHex = blockNumber ? `0x${blockNumber.toString(16)}` : "latest";

  const resp = await client.request({
    method: "debug_traceCall" as any,
    params: [
      {
        from: DUMMY_SENDER,
        to: ROUTER_ADDRESS,
        data: calldata,
        value: inputToken === NATIVE_TOKEN ? `0x${amountIn.toString(16)}` : undefined,
        gas: "0x5F5E100", // 100M
      },
      blockHex,
      {
        stateOverrides,
      },
    ] as any,
  });

  const result = resp as any;
  return {
    gas: BigInt(result.gas),
    failed: result.failed ?? false,
  };
}
