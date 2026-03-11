// Trace WooPP V2 with prestate overrides
import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { ROUTER_ADDRESS } from "hayabusa-router";
import { readFileSync } from "node:fs";
import { encodeSwap } from "../router/encode.ts";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const WOOPP = "0xABa7eD514217D51630053d73D358aC2502d3f9BB";
const ORACLE = "0xd92e3c8f1c5e835e4e76173c4e83bf517f61b737";
const FEED = "0x5ecf662abb8c2ab099862f9ef2ddc16cbc8a9977";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const amountIn = 4950000000n;
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

const bytecode = "0x" + readFileSync("/home/claude/defi-toolbox-avalanche/router/contracts/bytecode.hex", "utf-8").trim();
const { keccak256, encodeAbiParameters } = await import("viem");
const slot = keccak256(encodeAbiParameters([{type: "address"}, {type: "uint256"}], [ROUTER_ADDRESS as Hex, 9n]));

const route = [{
  pool: { address: WOOPP, providerName: "", poolType: 14 as any, tokens: [USDC, WAVAX], latestSwapBlock: 0 },
  tokenIn: USDC, tokenOut: WAVAX,
}];
const calldata = encodeSwap(route, amountIn);

const stateOverride = {
  [ROUTER_ADDRESS]: { code: bytecode, stateDiff: {} },
  [USDC]: { stateDiff: { [slot]: `0x${amountIn.toString(16).padStart(64, "0")}` } },
  [WOOPP]: { stateDiff: {
    "0x0000000000000000000000000000000000000000000000000000000000000000": "0x000000000000030d400000059dffffff388434c96d343042447fe596ad32fffe",
    "0x0000000000000000000000000000000000000000000000000000000000000001": "0x00000000000003dcf47dcd199a05ae13000000000000007e3fc38f25c3652b79",
    "0x0000000000000000000000000000000000000000000000000000000000000005": "0x000069b11cb40186a00186a0ef3690cbb1217410833f093ce56f7a1603787cad",
    "0x0000000000000000000000000000000000000000000000000000000000000007": "0x000000000000000000000000d92e3c8f1c5e835e4e76173c4e83bf517f61b737",
    "0x38b5b2ceac7637132d27514ffcf440b705287635075af7b8bd5adcaa6a4cc5bb": "0x00000000006400000000000000000000000000000000000fb5f9d55f136fd042",
    "0x8a2f9a362cf691f53af8c9abba033d2a64b68bfcee470b7430a52aff2060fb4a": "0x000000000064000000004df4a30cdc48ac44a20000000007b039e13794b8012a",
    "0xac33ff75c19e70fe83507db0d683fd3465c996598dc972688b7ace676c89077b": "0x00000000006400000000000000000000000000000000000fa266d739923aa6ff",
  }},
  [ORACLE]: { stateDiff: {
    "0x0000000000000000000000000000000000000000000000000000000000000001": "0x0000000000000000000000000000000000000000000000000000000000000000",
    "0x0000000000000000000000000000000000000000000000000000000000000004": "0x0000000000000000000000000000000000000000000000000de0b6b3a7640000",
    "0x0000000000000000000000000000000000000000000000000000000000000005": "0x0000000000000000000000000000000000000000000000000de0b6b3a7640000",
    "0x0000000000000000000000000000000000000000000000000000000000000006": "0x0000015e01f4007805dc00fa000000000000c350640507d00000000001f4ffff",
    "0x0000000000000000000000000000000000000000000000000000000000000007": "0x00000000000000000000000000000000000000000000000000000000000186a0",
  }},
  [FEED]: { stateDiff: {
    "0x173c8c80df9d81999c5929f8d222637fccb7b4bd430e861626bb981e1c1c2a46": "0x0000000000000000000000000000000000000000000000000000000000000003",
    "0xd47bbe930eed157ec8bd3c34c5836ada63283ee4000000000000000000000000": "0x5ddadcf400125ddadedc00331f1212e8000b1f13fb30000000019cdbd9fb0500",
    "0xfa1db1571892f8828743f8f95dcca11f3fe274b1de81125c34d79163e24a2001": "0xffffffffffffffffffffffffffffffff00000000000000000000000000000000",
  }},
};

const traceResult = await client.request({
  method: "debug_traceCall" as any,
  params: [
    { from: "0x000000000000000000000000000000000000dEaD", to: ROUTER_ADDRESS, data: calldata },
    blockHex,
    { tracer: "callTracer", stateOverrides: stateOverride },
  ] as any,
});

function printTrace(call: any, depth = 0) {
  const indent = "  ".repeat(depth);
  const from = (call.from || "").slice(0, 10);
  const to = (call.to || "").slice(0, 10);
  const input = (call.input || "").slice(0, 10);
  const value = call.value ? ` val=${BigInt(call.value)}` : "";
  const output = call.output ? ` out=${call.output.slice(0, 34)}` : "";
  const error = call.error ? ` ERROR=${call.error}` : "";
  console.log(`${indent}${call.type} ${from}→${to} ${input}${value}${output}${error}`);
  for (const sub of (call.calls || [])) printTrace(sub, depth + 1);
}
printTrace(traceResult);
