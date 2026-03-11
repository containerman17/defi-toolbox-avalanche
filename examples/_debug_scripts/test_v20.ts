import { createPublicClient, http, keccak256, toBytes } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

async function main() {
  const selTokenX = keccak256(toBytes("tokenX()")).slice(0, 10);
  const selTokenY = keccak256(toBytes("tokenY()")).slice(0, 10);
  const selGetTokenX = keccak256(toBytes("getTokenX()")).slice(0, 10);
  const selSwap = keccak256(toBytes("swap(bool,address)")).slice(0, 10);
  console.log("tokenX() selector:", selTokenX);
  console.log("tokenY() selector:", selTokenY);
  console.log("getTokenX() selector:", selGetTokenX);
  console.log("swap(bool,address) selector:", selSwap);
  
  // Try calling tokenX() on the V2.0 pool
  try {
    const result = await client.call({ to: "0x1d7a1a79e2b4ef88d2323f3845246d24a3c20f1d", data: selTokenX as `0x${string}` });
    console.log("tokenX() result:", result.data);
  } catch(e: any) {
    console.log("tokenX() error:", e.message?.slice(0, 100));
  }
  
  try {
    const result = await client.call({ to: "0x1d7a1a79e2b4ef88d2323f3845246d24a3c20f1d", data: selGetTokenX as `0x${string}` });
    console.log("getTokenX() result:", result.data);
  } catch(e: any) {
    console.log("getTokenX() error:", e.message?.slice(0, 100));
  }
}
main();
