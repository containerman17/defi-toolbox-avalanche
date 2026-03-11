// Test: manually inject WooPP pool + oracle storage to verify it fixes the simulation
import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { quoteRoute, ROUTER_ADDRESS } from "hayabusa-router";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const WOOPP = "0xABa7eD514217D51630053d73D358aC2502d3f9BB";
const ORACLE = "0xd92e3c8f1c5e835e4e76173c4e83bf517f61b737";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const amountIn = 4950000000n;
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

// Fetch WooPP pool slots at target block
const wooppSlots = [
  "0x0000000000000000000000000000000000000000000000000000000000000000",
  "0x0000000000000000000000000000000000000000000000000000000000000001",
  "0x0000000000000000000000000000000000000000000000000000000000000005",
  "0x0000000000000000000000000000000000000000000000000000000000000007",
  "0x38b5b2ceac7637132d27514ffcf440b705287635075af7b8bd5adcaa6a4cc5bb",
  "0x8a2f9a362cf691f53af8c9abba033d2a64b68bfcee470b7430a52aff2060fb4a",
  "0xac33ff75c19e70fe83507db0d683fd3465c996598dc972688b7ace676c89077b",
];

const oracleSlots = [
  "0x0000000000000000000000000000000000000000000000000000000000000001",
  "0x0000000000000000000000000000000000000000000000000000000000000004",
  "0x0000000000000000000000000000000000000000000000000000000000000005",
  "0x0000000000000000000000000000000000000000000000000000000000000006",
  "0x0000000000000000000000000000000000000000000000000000000000000007",
];

console.log("Fetching WooPP state at block", blockHex);
const wooppStorage: Record<Hex, Hex> = {};
for (const slot of wooppSlots) {
  const val = await client.request({
    method: "eth_getStorageAt" as any,
    params: [WOOPP, slot, blockHex] as any,
  });
  wooppStorage[slot as Hex] = val as Hex;
  console.log(`  WooPP[${slot.slice(0,10)}] = ${val}`);
}

const oracleStorage: Record<Hex, Hex> = {};
for (const slot of oracleSlots) {
  const val = await client.request({
    method: "eth_getStorageAt" as any,
    params: [ORACLE, slot, blockHex] as any,
  });
  oracleStorage[slot as Hex] = val as Hex;
  console.log(`  Oracle[${slot.slice(0,10)}] = ${val}`);
}

// Now try quoteRoute with these state overrides
const extraStateOverrides = {
  [WOOPP]: { stateDiff: wooppStorage },
  [ORACLE]: { stateDiff: oracleStorage },
};

const out = await quoteRoute(
  client,
  [{
    pool: {
      address: WOOPP,
      providerName: "",
      poolType: 14 as any,
      tokens: [USDC, WAVAX],
      latestSwapBlock: 0,
    },
    tokenIn: USDC,
    tokenOut: WAVAX,
  }],
  amountIn,
  block,
  extraStateOverrides
);
console.log(`\nquoteRoute output with storage overrides: ${out}`);
console.log(`Expected ~519M WAVAX (from real tx split)`);
