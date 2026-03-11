// Check WooPP V2 broker authorization and storage slots
import { createPublicClient, http, keccak256, encodeAbiParameters, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { ROUTER_ADDRESS } from "hayabusa-router";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const LFJ_ROUTER = "0xb4315e873dBcf96Ffd0acd8EA43f689D8c20fB30";
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

const txHash = "0x1f84c56947f5d4e8d12096ea6c4622f368405b87f96f0924d8775453cea4bbff";

// Get real prestate for WooPP to see what storage slots are accessed
console.log("=== Real tx prestate ===");
const prestateTrace = await client.request({
  method: "debug_traceTransaction" as any,
  params: [txHash, { tracer: "prestateTracer", tracerConfig: { diffMode: false } }] as any,
});

const prestate = prestateTrace as any;
const wooppKey = Object.keys(prestate).find(k => k.toLowerCase() === WOOPP.toLowerCase());
if (wooppKey) {
  const storage = prestate[wooppKey].storage || {};
  console.log(`WooPP has ${Object.keys(storage).length} storage slots:`);
  for (const [slot, val] of Object.entries(storage)) {
    console.log(`  ${slot} = ${val}`);
  }
} else {
  console.log("WooPP NOT found in prestate keys:", Object.keys(prestate).filter(k => k.includes("aba")));
}

// Check diffMode to see what slots CHANGED
console.log("\n=== Diff trace (slots changed by WooPP) ===");
const diffTrace = await client.request({
  method: "debug_traceTransaction" as any,
  params: [txHash, { tracer: "prestateTracer", tracerConfig: { diffMode: true } }] as any,
}) as any;

const preKey = Object.keys(diffTrace.pre || {}).find((k: string) => k.toLowerCase() === WOOPP.toLowerCase());
const postKey = Object.keys(diffTrace.post || {}).find((k: string) => k.toLowerCase() === WOOPP.toLowerCase());
if (preKey) {
  console.log("WooPP pre:", JSON.stringify(diffTrace.pre[preKey].storage, null, 2));
}
if (postKey) {
  console.log("WooPP post:", JSON.stringify(diffTrace.post[postKey].storage, null, 2));
}

// Try to check if LFJ_ROUTER is whitelisted as a broker
// brokers mapping is probably storage slot something, key = address
// Common pattern: mapping(address=>bool) brokers at slot N
// Compute slot = keccak256(abi.encode(address, slotN))
console.log("\n=== Broker whitelist check ===");

// Try slots 0-20 for broker mapping
for (let slotN = 0n; slotN <= 20n; slotN++) {
  const lfjSlot = keccak256(encodeAbiParameters(
    [{ type: "address" }, { type: "uint256" }],
    [LFJ_ROUTER as Hex, slotN]
  ));
  const lfjVal = await client.getStorageAt({
    address: WOOPP as `0x${string}`,
    slot: lfjSlot as `0x${string}`,
    blockNumber: block,
  });
  if (lfjVal && lfjVal !== "0x0000000000000000000000000000000000000000000000000000000000000000") {
    console.log(`Slot ${slotN}: LFJ_ROUTER broker mapping = ${lfjVal}`);
  }

  const routerSlot = keccak256(encodeAbiParameters(
    [{ type: "address" }, { type: "uint256" }],
    [ROUTER_ADDRESS as Hex, slotN]
  ));
  const routerVal = await client.getStorageAt({
    address: WOOPP as `0x${string}`,
    slot: routerSlot as `0x${string}`,
    blockNumber: block,
  });
  if (routerVal && routerVal !== "0x0000000000000000000000000000000000000000000000000000000000000000") {
    console.log(`Slot ${slotN}: ROUTER broker mapping = ${routerVal}`);
  }
}

// Direct read slots 0-15
console.log("\n=== WooPP direct storage slots 0-15 ===");
for (let i = 0; i <= 15; i++) {
  const slot = `0x${i.toString(16).padStart(64, "0")}` as `0x${string}`;
  const val = await client.getStorageAt({ address: WOOPP as `0x${string}`, slot, blockNumber: block });
  if (val && val !== "0x0000000000000000000000000000000000000000000000000000000000000000") {
    console.log(`  slot ${i}: ${val}`);
  }
}
