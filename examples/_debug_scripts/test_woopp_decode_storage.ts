// Decode WooPP V2 storage slots to understand what changed during the swap
import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const block = BigInt(80108339) - 1n;

// Pre-state storage:
// slot 0: 0x000000000000030d400000059dffffff388434c96d343042447fe596ad32fffe
// slot 1: 0x00000000000003dcf47dcd199a05ae13000000000000007e3fc38f25c3652b79
// slot 5: 0x000069b11cb40186a00186a0ef3690cbb1217410833f093ce56f7a1603787cad
// slot 7: 0x000000000000000000000000d92e3c8f1c5e835e4e76173c4e83bf517f61b737
// Hash slot 38b5b2ce = 0x00000000006400000000000000000000000000000000000fb5f9d55f136fd042 (WAVAX info)
// Hash slot 8a2f9a36 = 0x000000000064000000004df4a30cdc48ac44a20000000007b039e13794b8012a (USDC info)
// Hash slot ac33ff75 = 0x00000000006400000000000000000000000000000000000fa266d739923aa6ff

// Post-state changes:
// slot 38b5b2ce -> 0x00000000006400000000953a029d23c6fb2da700000000000000000000000000 (WAVAX reserve changed!)
// slot 8a2f9a36 -> 0x0000000000640000000096fa16e17dbb4cbebb00000000000000000000000000 (USDC reserve changed!)
// slot ac33ff75 -> 0x000000000064000000002e10aa7f2142412d76000000000ac8ee1b07fb39d112

// Decode slot 38b5b2ce pre:
const wavaxPre = 0x00000000006400000000000000000000000000000000000fb5f9d55f136fd042n;
// High 16 bytes: 0x0000000000640000000000000000000000000000 (fee = 100 = 0x64; reserve+spread?)
// Low 16 bytes: 0x000000000000000fb5f9d55f136fd042 = 1114050003497984066n = ~1114 WAVAX (18 dec)
const wavaxReservePre = wavaxPre & ((1n << 128n) - 1n);
console.log("WAVAX reserve pre (low 128 bits):", wavaxReservePre);
console.log("WAVAX reserve pre (human):", Number(wavaxReservePre) / 1e18);

// Post state: low 128 bits = 0 (reserve was withdrawn to router)
const wavaxPost = 0x00000000006400000000953a029d23c6fb2da700000000000000000000000000n;
const wavaxReservePost = wavaxPost & ((1n << 128n) - 1n);
console.log("WAVAX reserve post (low 128 bits):", wavaxReservePost);
console.log("WAVAX reserve post (human):", Number(wavaxReservePost) / 1e18);

// What if low 128 bits is NOT the reserve but something else?
// Let's look at the high 128 bits
const wavaxPreHigh = wavaxPre >> 128n;
const wavaxPostHigh = wavaxPost >> 128n;
console.log("\nWAVAX high 128 bits pre:", wavaxPreHigh);
console.log("WAVAX high 128 bits post:", wavaxPostHigh);

// Decode USDC slot
const usdcPre = 0x000000000064000000004df4a30cdc48ac44a20000000007b039e13794b8012an;
const usdcPost = 0x0000000000640000000096fa16e17dbb4cbebb00000000000000000000000000n;
const usdcReservePre = usdcPre & ((1n << 128n) - 1n);
const usdcReservePost = usdcPost & ((1n << 128n) - 1n);
console.log("\nUSDC reserve pre (low 128 bits):", usdcReservePre);
console.log("USDC reserve pre (human, 6 dec):", Number(usdcReservePre) / 1e6);
console.log("USDC reserve post (low 128 bits):", usdcReservePost);

// The tx swapped USDC→WAVAX: USDC went IN, WAVAX went OUT
// If reserves are in high bits, let's check
const usdcPreHigh = usdcPre >> 128n;
const usdcPostHigh = usdcPost >> 128n;
console.log("\nUSDC high 128 bits pre:", usdcPreHigh);
console.log("USDC high 128 bits post:", usdcPostHigh);
console.log("USDC delta high:", usdcPostHigh - usdcPreHigh);

// Slot 0 decode (high 24 bits packed)
const slot0Pre = 0x000000000000030d400000059dffffff388434c96d343042447fe596ad32fffen;
const slot0Post = 0x000000000000030d4000004614000000004f6810c1fe373a09b595445df00000n;
console.log("\nSlot 0 high 64 bits pre:", slot0Pre >> 192n);
console.log("Slot 0 high 64 bits post:", slot0Post >> 192n);
// bits 128-192
console.log("Slot 0 mid 64 bits pre:", (slot0Pre >> 128n) & 0xffffffffffffffffn);
console.log("Slot 0 mid 64 bits post:", (slot0Post >> 128n) & 0xffffffffffffffffn);

// The key question: what is the `data` parameter that WooPP uses?
// Looking at the prestate trace, slot 7 = oracle address (0xd92e3c8f...)
// But prestate only includes slots accessed: slot 0, 1, 5, 7 + 3 hash-based slots
// That means there's NO broker whitelist check!

console.log("\n=== Checking slot 7 (oracle addr) ===");
const slot7 = await client.getStorageAt({
  address: WOOPP as `0x${string}`,
  slot: "0x0000000000000000000000000000000000000000000000000000000000000007",
  blockNumber: block,
});
console.log("slot 7:", slot7);

// Check if WooPP uses sender address for broker fee — let's look at slot 5
const slot5 = 0x000069b11cb40186a00186a0ef3690cbb1217410833f093ce56f7a1603787cad0n >> 0n;
console.log("\nSlot 5:", slot5.toString(16));
// slot 5 high bits: 0x000069b11cb4 = timestamp-ish?
// Lower 20 bytes: ef3690cbb1217410833f093ce56f7a1603787cad = ??? (fee receiver?)
// Also: 0186a00186a0 = fee params?

// What address is ef3690cbb1217410833f093ce56f7a1603787cad?
console.log("\nWhat is 0xef3690cbb1217410833f093ce56f7a1603787cad?");
console.log("This appears in slot 5 and slot 6, likely a fee address or admin");

// The real question: why does WooPP not send WAVAX to the router?
// The prestate shows slot 38b5b2ce has the WAVAX "reserve" or balance in some form
// Post-state shows it going to 0 (meaning WAVAX was SENT to someone)
// But in our eth_call simulation, is the WAVAX balance actually in WooPP?

// Check WooPP's actual WAVAX balance at block
const WAVAX_ERC20 = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const balanceOfSel = "0x70a08231" + WOOPP.slice(2).padStart(64, "0");
const wavaxBalance = await client.request({
  method: "eth_call" as any,
  params: [{ to: WAVAX_ERC20, data: balanceOfSel }, `0x${block.toString(16)}`] as any,
});
console.log("\nWooPP WAVAX balance at block:", BigInt(wavaxBalance as string));
console.log("WooPP WAVAX balance (human):", Number(BigInt(wavaxBalance as string)) / 1e18);
