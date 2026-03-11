import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const RPC_URL = process.env.RPC_URL || "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(RPC_URL) });

// The two pools in the original trace
const TARGET_POOLS = [
  "0xed2a7edd7413021d440b09d654f3b87712abab66", // USDC.e → LP token (0xcfc37a6a)
  "0xa196a03653f6cc5ca0282a8bd7ec60e93f620afc", // LP token → USDt
];

async function main() {
  // Check what 0xcfc37a6a is
  console.log("Checking token 0xcfc37a6a...");
  try {
    // Name
    const nameData = await client.call({
      to: "0xcfc37a6ab183dd2adc73cd5763be8a52b3e2b35a" as any,
      data: "0x06fdde03" as any, // name()
    });
    console.log("Token name data:", nameData);
  } catch (e: any) {
    console.log("Error:", e.message?.slice(0, 100));
  }
  
  // Let's look at pool info for 0xed2a7edd
  console.log("\nChecking 0xed2a7edd (first platypus pool)...");
  try {
    const data = await client.call({
      to: "0xed2a7edd7413021d440b09d654f3b87712abab66" as any,
      data: "0x06fdde03" as any, // name()
    });
    console.log("Name result:", data);
  } catch (e: any) {
    console.log("Error:", e.message?.slice(0, 100));
  }
  
  // Check 0xa196a03 
  console.log("\nChecking 0xa196a03 (second platypus pool)...");
  try {
    const data = await client.call({
      to: "0xa196a03653f6cc5ca0282a8bd7ec60e93f620afc" as any,
      data: "0x06fdde03" as any, // name()
    });
    console.log("Name result:", data);
  } catch (e: any) {
    console.log("Error:", e.message?.slice(0, 100));
  }
}

main().catch(console.error);
