// Test: does state override affect unrelated contracts' storage?
import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { keccak256, encodeAbiParameters } from "viem";
import { ROUTER_ADDRESS } from "hayabusa-router";
import { readFileSync } from "node:fs";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const WOOPP = "0xABa7eD514217D51630053d73D358aC2502d3f9BB";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

const bytecode = "0x" + readFileSync("/home/claude/defi-toolbox-avalanche/router/contracts/bytecode.hex", "utf-8").trim();
const slot = keccak256(encodeAbiParameters([{type: "address"}, {type: "uint256"}], [ROUTER_ADDRESS as Hex, 9n]));

// Check WooPP slot 0 WITHOUT state override
const woopp_slot0_bare = await client.request({
  method: "eth_call" as any,
  params: [{ to: WOOPP, data: "0x54" }, blockHex] as any,
}).catch(e => "err");
console.log("WooPP SLOAD(0) bare call:", woopp_slot0_bare);

// Read WooPP slot 0 via eth_getStorageAt  
const woopp_slot0 = await client.getStorageAt({ 
  address: WOOPP as `0x${string}`, 
  slot: "0x0000000000000000000000000000000000000000000000000000000000000000" as `0x${string}`,
  blockNumber: block
});
console.log("WooPP slot 0 via getStorageAt:", woopp_slot0);

// Read WooPP slot 0 via eth_call with state override for DIFFERENT contract (ROUTER + USDC)
const stateOverride = {
  [ROUTER_ADDRESS]: { code: bytecode as Hex },
  [USDC]: { stateDiff: { [slot]: `0x${(4950000000n).toString(16).padStart(64, "0")}` as Hex } },
};

// Check WooPP slot 0 WITH state override for different contracts
// Use a simple call: call WooPP's storage slot 0
// Actually let me call the oracle to see if it returns the right value WITH overrides
const ORACLE = "0xd92e3c8f1c5e835e4e76173c4e83bf517f61b737";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";

const oracle_without = await client.request({
  method: "eth_call" as any,
  params: [{ to: ORACLE, data: "0xc1701b67" + WAVAX.slice(2).padStart(64, "0") }, blockHex] as any,
});

const oracle_with = await client.request({
  method: "eth_call" as any,
  params: [{ to: ORACLE, data: "0xc1701b67" + WAVAX.slice(2).padStart(64, "0") }, blockHex, stateOverride] as any,
});

console.log("\nOracle result WITHOUT override:", oracle_without);
console.log("Oracle result WITH override:", oracle_with);
console.log("Same?", oracle_without === oracle_with);

// Now check: call WooPP.swap with the state override - trace it
// Use callTracer to see if slot 0 is being read correctly
const trace = await client.request({
  method: "debug_traceCall" as any,
  params: [
    { from: "0x000000000000000000000000000000000000dEaD", to: ORACLE, 
      data: "0xc1701b67" + WAVAX.slice(2).padStart(64, "0") },
    blockHex,
    { tracer: "prestateTracer", stateOverrides: stateOverride, tracerConfig: { diffMode: false } },
  ] as any,
}) as any;
console.log("\nPrestate for ORACLE in debug_traceCall with stateOverrides:");
const oracleState = trace[ORACLE.toLowerCase()];
console.log("Oracle storage:", JSON.stringify(oracleState?.storage || {}, null, 2));
