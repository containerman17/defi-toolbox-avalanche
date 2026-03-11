import { createPublicClient, webSocket, type Hex, decodeAbiParameters, encodeAbiParameters } from "viem";
import { avalanche } from "viem/chains";
import { readFileSync } from "fs";
import { join } from "path";

const client = createPublicClient({ chain: avalanche, transport: webSocket("ws://localhost:9650/ext/bc/C/ws") });

const ROUTER_ADDRESS = "0x2bef1becdafcfe8990a233d03a98bbb39021c96e";
const DUMMY_SENDER = "0x000000000000000000000000000000000000dEaD";

function getRouterBytecode(): Hex {
  const hex = readFileSync(join(import.meta.dirname!, "../router/contracts/bytecode.hex"), "utf-8").trim();
  return `0x${hex}` as Hex;
}

// Test Synapse: USDt.e → nUSD via pool 0xed2a7edd, indices from=3,to=0
async function testSynapse() {
  console.log("=== Testing Synapse ===");
  const pool = "0xed2a7edd7413021d440b09d654f3b87712abab66";
  const tokenIn = "0xc7198437980c041c805a1edcba50c1ce5db95118"; // USDt.e (index 3)
  const tokenOut = "0xcfc37a6ab183dd4aed08c204d1c2773c0b1bdf46"; // nUSD (index 0)
  const amountIn = 1000000n; // 1 USDt.e (6 decimals)

  // Encode extraData: abi.encode(uint8, uint8) for from=3, to=0
  const extraData = encodeAbiParameters([{ type: "uint8" }, { type: "uint8" }], [3, 0]);
  console.log("extraData:", extraData);

  // Build swap calldata
  const { encodeSwapFlat } = await import("../router/encode.ts");
  // Can't use encodeSwapFlat directly, let's do a raw eth_call

  // Try calling the pool directly to see if swap works
  try {
    const result = await client.readContract({
      address: pool as Hex,
      abi: [{ name: "getToken", type: "function", inputs: [{ type: "uint8" }], outputs: [{ type: "address" }] }],
      functionName: "getToken",
      args: [3],
      blockNumber: 80160012n,
    });
    console.log("getToken(3):", result);
  } catch (e: any) {
    console.error("getToken failed:", e.message?.slice(0, 200));
  }

  // Try calculateSwap to verify the pool works
  try {
    const result = await client.readContract({
      address: pool as Hex,
      abi: [{ name: "calculateSwap", type: "function", inputs: [{ type: "uint8" }, { type: "uint8" }, { type: "uint256" }], outputs: [{ type: "uint256" }] }],
      functionName: "calculateSwap",
      args: [3, 0, amountIn],
      blockNumber: 80160012n,
    });
    console.log("calculateSwap(3, 0, 1000000):", result);
  } catch (e: any) {
    console.error("calculateSwap failed:", e.message?.slice(0, 200));
  }
}

// Test Trident: WAVAX → USDC via pool 0x895114d1, bento=0x0711b602
async function testTrident() {
  console.log("\n=== Testing Trident ===");
  const pool = "0x895114d100b5013b700f03900f825625d7db35cc";
  const bentoBox = "0x0711b6026068f736bae6b213031fce978d48e026";

  // Check pool exists and has tokens
  try {
    const token0 = await client.readContract({
      address: pool as Hex,
      abi: [{ name: "token0", type: "function", inputs: [], outputs: [{ type: "address" }] }],
      functionName: "token0",
      blockNumber: 80114945n,
    });
    const token1 = await client.readContract({
      address: pool as Hex,
      abi: [{ name: "token1", type: "function", inputs: [], outputs: [{ type: "address" }] }],
      functionName: "token1",
      blockNumber: 80114945n,
    });
    console.log("token0:", token0);
    console.log("token1:", token1);
  } catch (e: any) {
    console.error("token calls failed:", e.message?.slice(0, 200));
  }
}

await testSynapse();
await testTrident();
process.exit(0);
