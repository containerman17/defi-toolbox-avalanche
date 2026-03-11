// Check if oracle returns feasible=true when called from WooPP context
import { createPublicClient, http, decodeAbiParameters, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { getBalanceOverride, getAllowanceOverride, ROUTER_ADDRESS } from "hayabusa-router";
import { readFileSync } from "node:fs";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const BYTECODE = "0x" + readFileSync("/home/claude/defi-toolbox-avalanche/router/contracts/bytecode.hex", "utf-8").trim();

const ORACLE = "0xd92e3c8f1c5e835e4e76173c4e83bf517f61b737";
const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const amountIn = 4950000000n;
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;
const DUMMY_SENDER = "0x000000000000000000000000000000000000dEaD";

// Oracle selector 0xc1701b67(address baseToken)
// Call it for WAVAX - first with no overrides to confirm it returns real data
const oracleCallData = "0xc1701b67" + WAVAX.slice(2).padStart(64, "0");

// Direct oracle call, no overrides
const oracleResult = await client.request({
  method: "eth_call" as any,
  params: [{ to: ORACLE, data: oracleCallData }, blockHex] as any,
});
const [price, feasible] = decodeAbiParameters([{type:"uint256"},{type:"uint256"}], oracleResult as Hex);
console.log("Oracle result (no overrides): price=", price, "feasible=", feasible);

// Build state override (same as in quoteRoute)
const balOverride = getBalanceOverride(USDC, amountIn, DUMMY_SENDER);
const allowOverride = getAllowanceOverride(USDC, DUMMY_SENDER, ROUTER_ADDRESS);
const merged: Record<string, Record<string, Hex>> = {};
for (const ovr of [balOverride, allowOverride]) {
  for (const [addr, val] of Object.entries(ovr)) {
    if (!merged[addr]) merged[addr] = {};
    Object.assign(merged[addr], val.stateDiff);
  }
}
const stateOverride: Record<string, any> = {};
for (const [address, slots] of Object.entries(merged)) {
  stateOverride[address] = { stateDiff: slots };
}
stateOverride[ROUTER_ADDRESS] = { code: BYTECODE as Hex };

// Oracle call with state overrides
const oracleResultWith = await client.request({
  method: "eth_call" as any,
  params: [{ to: ORACLE, data: oracleCallData }, blockHex, stateOverride] as any,
});
const [priceWith, feasibleWith] = decodeAbiParameters([{type:"uint256"},{type:"uint256"}], oracleResultWith as Hex);
console.log("Oracle result (with overrides): price=", priceWith, "feasible=", feasibleWith);

// But wait: the oracle returned ALL ZEROS in the callTracer trace!
// Let me check: does oracle return 0 when called from WooPP (msg.sender = WooPP)?
// Or is it a problem with the callTracer showing "0x00..." for all outputs?

// Actually looking at the callTracer output: output: 0x00000000000000000000000000000000
// That looks like 16 bytes of zeros, not the real oracle response
// The oracle called WITHOUT stateOverrides returns the correct value above

// The callTracer might show truncated output. Let me check the actual WooPP swap output
// by looking at what WooPP does after oracle returns:
// WooPP swap flow (inferred from standard WooPP V2 code):
// 1. Get price from oracle → if feasible=false, return
// 2. Compute output amount
// 3. Transfer tokenOut to recipient (flash swap: SEND FIRST, then callback)
// 4. Call callback
// 5. Verify tokenIn received

// Key insight: in WooPP V2 flash-swap pattern:
// The pool sends tokenOut FIRST, then calls callback to pull tokenIn
// So WooPP should transfer WAVAX to router BEFORE calling the callback
// But in our trace, the callback is called and USDC is sent, but NO WAVAX transfer from WooPP!

// Is WooPP checking if "feasible" internally and skipping the transfer?
// Or is the output amount computed as 0?

// Let me trace the WooPP call specifically to see what it does
const wooppSwapCalldata = "0xac8bb7d9" +
  ROUTER_ADDRESS.slice(2).padStart(64, "0") + // broker = ROUTER
  "0".padStart(64, "0") + // direction = 0 (sell quote = USDC→WAVAX)
  amountIn.toString(16).padStart(64, "0") + // amount
  "0".padStart(64, "0") + // minOutput = 0
  "a0".padStart(64, "0") + // bytes offset
  "20".padStart(64, "0") + // bytes length
  USDC.slice(2).padStart(64, "0"); // data = tokenIn (USDC)

// Trace the WooPP call directly with state overrides
const wooppTrace = await client.request({
  method: "debug_traceCall" as any,
  params: [
    { from: ROUTER_ADDRESS, to: WOOPP, data: wooppSwapCalldata, gas: "0x989680" },
    blockHex,
    { tracer: "callTracer", tracerConfig: { withLog: true }, stateOverrides: stateOverride },
  ] as any,
}) as any;

function printTrace(call: any, depth = 0) {
  const indent = "  ".repeat(depth);
  const from = (call.from || "").slice(0, 12);
  const to = (call.to || "").slice(0, 12);
  const input = (call.input || "").slice(0, 18);
  const output = call.output ? ` out=${call.output.slice(0, 66)}` : "";
  const err = call.error ? ` ERR:${call.error}` : "";
  console.log(`${indent}${call.type} ${from}→${to} ${input}${output}${err}`);
  for (const log of (call.logs || [])) {
    const topics = (log.topics || []).map((t: string) => t.slice(0, 10)).join(",");
    console.log(`${indent}  LOG ${(log.address || "").slice(0, 12)} topics=${topics} data=${(log.data || "").slice(0, 66)}`);
  }
  for (const sub of (call.calls || [])) printTrace(sub, depth + 1);
}

console.log("\n=== WooPP direct call trace (from ROUTER with state overrides) ===");
printTrace(wooppTrace);
console.log("\nError:", wooppTrace.error);
console.log("Output:", wooppTrace.output?.slice(0, 66));
