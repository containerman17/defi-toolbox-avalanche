// Debug script to understand why bridge-skip doesn't trigger
import * as fs from "node:fs";
import * as path from "node:path";

const payload = JSON.parse(fs.readFileSync(
  path.join(import.meta.dirname!, "payloads", "0x447a7f6aca1d7593abec732829422fb0ab3bce338f6b628945cb0f02f6f7cc2c.json"),
  "utf-8"
));

const BRIDGE_EQUIVALENTS: Record<string, string> = {
  "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664": "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e", // USDC.e → USDC
  "0xc7198437980c041c805a1edcba50c1ce5db95118": "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7", // USDt.e → USDt
  "0x0000000000000000000000000000000000000000": "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7", // AVAX → WAVAX
};

const step1 = payload.steps[0];
console.log("Step 1 route:", step1.route);
console.log("Step 1 tokens:", step1.tokens);
console.log("Step 1 pools:", step1.pools);
console.log("Step 1 poolTypes:", step1.poolTypes);

// The issue: step1 already has all 3 hops in its 'pools' array.
// But what did convert.ts construct? Let me check:
// pools[0] = algebra (LINK→USDC)
// pools[1] = uniswap_v3 (USDC→USDC.e)
// pools[2] = platypus (USDC.e→USDt)

// The "step" in convert.ts chainMap would be algebra(LINK→USDC)
// The "downStep" would be uniswap_v3(USDC→USDC.e)
// BRIDGE_EQUIVALENTS[downStep.tokenOut="0xa7d7079b..."] = "0xb97ef9ef..."
// step.tokenOut = "0xb97ef9ef..."

// So the condition should trigger IF:
// - downStep.tokenOut = USDC.e = "0xa7d7079b..."
// - step.tokenOut = USDC = "0xb97ef9ef..."
// - BRIDGE_EQUIVALENTS["0xa7d7079b..."] = "0xb97ef9ef..."
// - bridgeCanonical === step.tokenOut → "0xb97ef9ef..." === "0xb97ef9ef..." → TRUE!

// But wait - what if the addresses in the trace are NOT lowercase?

console.log("\nChecking address cases...");
console.log("downStep.tokenOut (USDC.e from payload):", step1.tokens[2]); // index 2 is USDC.e
console.log("Expected USDC.e address:", "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664");
console.log("Match:", step1.tokens[2].toLowerCase() === "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664");

// Check BRIDGE_EQUIVALENTS key lookup
const testKey = step1.tokens[2];
console.log("\nDirect lookup:", BRIDGE_EQUIVALENTS[testKey]);
console.log("Lowercase lookup:", BRIDGE_EQUIVALENTS[testKey.toLowerCase()]);
