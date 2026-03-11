import { createPublicClient, webSocket } from "viem";
import { avalanche } from "viem/chains";
import { quoteRoute } from "../router/quote.ts";
import type { StoredPool } from "../pools/types.ts";

const client = createPublicClient({ chain: avalanche, transport: webSocket("ws://localhost:9650/ext/bc/C/ws") });

// Test Synapse route: JOE → USDt.e → nUSD → USDC
const synRoute = [
  { pool: { address: "0xd50a48b107866f53e7ddeda9b96a787d0e9b8bf5", providerName: "", poolType: 8 as const, tokens: ["0x6e84a6216ea6dacc71ee8e6b0a5b7322eebc0fdd", "0xc7198437980c041c805a1edcba50c1ce5db95118"], latestSwapBlock: 0 }, tokenIn: "0x6e84a6216ea6dacc71ee8e6b0a5b7322eebc0fdd", tokenOut: "0xc7198437980c041c805a1edcba50c1ce5db95118" },
  { pool: { address: "0xed2a7edd7413021d440b09d654f3b87712abab66", providerName: "", poolType: 19 as const, tokens: ["0xc7198437980c041c805a1edcba50c1ce5db95118", "0xcfc37a6ab183dd4aed08c204d1c2773c0b1bdf46"], latestSwapBlock: 0, extraData: "from=3,to=0" }, tokenIn: "0xc7198437980c041c805a1edcba50c1ce5db95118", tokenOut: "0xcfc37a6ab183dd4aed08c204d1c2773c0b1bdf46" },
  { pool: { address: "0xa196a03653f6cc5ca0282a8bd7ec60e93f620afc", providerName: "", poolType: 19 as const, tokens: ["0xcfc37a6ab183dd4aed08c204d1c2773c0b1bdf46", "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e"], latestSwapBlock: 0, extraData: "from=0,to=1" }, tokenIn: "0xcfc37a6ab183dd4aed08c204d1c2773c0b1bdf46", tokenOut: "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e" },
];

console.log("=== Testing Synapse route ===");
try {
  const out = await quoteRoute(client, synRoute as any, 5036223520000000000n, 80160012n);
  console.log("Output:", out);
} catch (e: any) {
  console.error("Error:", e.message?.slice(0, 500));
}

// Test Trident route: USDt → WAVAX → USDC
const triRoute = [
  { pool: { address: "0x4c4af8dbc524681930a27b2f1af5bcc8062e6fb7", providerName: "", poolType: 5 as const, tokens: ["0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7", "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7"], latestSwapBlock: 0 }, tokenIn: "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7", tokenOut: "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7" },
  { pool: { address: "0x895114d100b5013b700f03900f825625d7db35cc", providerName: "", poolType: 20 as const, tokens: ["0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7", "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e"], latestSwapBlock: 0, extraData: "bento=0x0711b6026068f736bae6b213031fce978d48e026" }, tokenIn: "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7", tokenOut: "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e" },
];

console.log("\n=== Testing Trident route ===");
try {
  const out = await quoteRoute(client, triRoute as any, 524229608603175n, 80114945n);
  console.log("Output:", out);
} catch (e: any) {
  console.error("Error:", e.message?.slice(0, 500));
}

process.exit(0);
