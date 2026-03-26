import { type PublicClient, type Hex, decodeAbiParameters } from "viem";
import { encodeSwap, encodeSwapFlat, type RouteStep, type FlatStep } from "./encode.ts";
import { buildStateOverrides, getBalanceOverrideAsync, getHookOverrides, isReflectionToken } from "./overrides.ts";
import addressJson from "./contracts/address.json" with { type: "json" };

// Single source of truth: imported from contracts/address.json (shared with Go via go:embed)
export const ROUTER_ADDRESS = addressJson.address.toLowerCase();

const DUMMY_SENDER = "0x000000000000000000000000000000000000dEaD";
const NATIVE_TOKEN = "0x0000000000000000000000000000000000000000";

export async function quoteRoute(
  client: PublicClient,
  route: RouteStep[],
  amountIn: bigint,
  blockNumber?: bigint,
  extraStateOverrides?: Record<string, any>,
  routerAddress: string = ROUTER_ADDRESS,
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
    const reflOvr = await getBalanceOverrideAsync(client, inputToken, amountIn, routerAddress, blockNumber);
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
  const tokenAmounts = new Map<string, bigint>([[inputToken, amountIn]]);
  const stateOverride = buildStateOverrides({
    routerAddress,
    tokenAmounts,
    extraStateOverrides: hasExtra ? mergedExtra : undefined,
  });

  const result = await client.request({
    method: "eth_call" as any,
    params: [
      {
        from: DUMMY_SENDER,
        to: routerAddress,
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
  routerAddress: string = ROUTER_ADDRESS,
): Promise<bigint> {
  if (steps.length === 0) throw new Error("empty steps");

  const calldata = encodeSwapFlat(steps);
  const blockHex = blockNumber ? `0x${blockNumber.toString(16)}` : "latest";

  // Aggregate required balances per input token from all steps with explicit amountIn > 0
  const tokenAmounts = new Map<string, bigint>();
  for (const step of steps) {
    if (step.amountIn > 0n) {
      const token = step.tokenIn.toLowerCase();
      tokenAmounts.set(token, (tokenAmounts.get(token) ?? 0n) + step.amountIn);
    }
  }
  // Ensure the primary input token is included
  const normalizedInput = inputToken.toLowerCase();
  if (!tokenAmounts.has(normalizedInput) && totalAmountIn > 0n) {
    tokenAmounts.set(normalizedInput, totalAmountIn);
  }

  // Collect all tokens for hook overrides
  const allTokens = new Set<string>();
  for (const step of steps) {
    allTokens.add(step.tokenIn.toLowerCase());
    allTokens.add(step.tokenOut.toLowerCase());
  }

  // Build extra overrides for reflection tokens and hook contracts
  const mergedExtra: Record<string, any> = { ...(extraStateOverrides ?? {}) };
  for (const [token, amount] of tokenAmounts) {
    if (token !== NATIVE_TOKEN && isReflectionToken(token)) {
      const reflOvr = await getBalanceOverrideAsync(client, token, amount, routerAddress, blockNumber);
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
  const stateOverride = buildStateOverrides({
    routerAddress,
    tokenAmounts,
    extraStateOverrides: hasExtra ? mergedExtra : undefined,
  });

  const result = await client.request({
    method: "eth_call" as any,
    params: [
      {
        from: DUMMY_SENDER,
        to: routerAddress,
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
 * Estimate gas for a swap via eth_estimateGas with state overrides.
 */
export async function estimateRouteGas(
  client: PublicClient,
  route: RouteStep[],
  amountIn: bigint,
  blockNumber?: bigint,
  routerAddress: string = ROUTER_ADDRESS,
): Promise<bigint> {
  if (route.length === 0) throw new Error("empty route");

  const calldata = encodeSwap(route, amountIn);
  const inputToken = route[0].tokenIn.toLowerCase();
  const tokenAmounts = new Map<string, bigint>([[inputToken, amountIn]]);
  const stateOverride = buildStateOverrides({ routerAddress, tokenAmounts });
  const blockHex = blockNumber ? `0x${blockNumber.toString(16)}` : "latest";

  const resp = await client.request({
    method: "eth_estimateGas" as any,
    params: [
      {
        from: DUMMY_SENDER,
        to: routerAddress,
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
 */
export async function traceRouteGas(
  client: PublicClient,
  route: RouteStep[],
  amountIn: bigint,
  blockNumber?: bigint,
  routerAddress: string = ROUTER_ADDRESS,
): Promise<{ gas: bigint; failed: boolean }> {
  if (route.length === 0) throw new Error("empty route");

  const calldata = encodeSwap(route, amountIn);
  const inputToken = route[0].tokenIn.toLowerCase();
  const tokenAmounts = new Map<string, bigint>([[inputToken, amountIn]]);
  const stateOverrides = buildStateOverrides({ routerAddress, tokenAmounts });
  const blockHex = blockNumber ? `0x${blockNumber.toString(16)}` : "latest";

  const resp = await client.request({
    method: "debug_traceCall" as any,
    params: [
      {
        from: DUMMY_SENDER,
        to: routerAddress,
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
