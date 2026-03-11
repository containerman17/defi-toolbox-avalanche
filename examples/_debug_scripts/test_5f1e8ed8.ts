// Investigate 0x5f1e8ed8 pool
import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const POOL = "0x5f1e8ed8468232bab71eda9f4598bda3161f48ea";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";

// From 0x47e00934: the tx uses this pool for USDC→WAVAX
// block 80127543, step 3 in tx but from logs: USDC→POOL→WAVAX
const block = BigInt(80127543) - 1n;
const blockHex = `0x${block.toString(16)}`;

const code = await client.getBytecode({ address: POOL as `0x${string}`, blockNumber: block });
console.log("Code length:", code?.length ?? 0);

// Try calling swap(address,address,uint256,uint256) = 0xfe029156
const swapData = "0xfe029156" +
  USDC.slice(2).padStart(64, "0") +
  WAVAX.slice(2).padStart(64, "0") +
  (2500000n).toString(16).padStart(64, "0") +
  "0".padStart(64, "0");
const r1 = await client.request({
  method: "eth_call" as any,
  params: [{ to: POOL, data: swapData }, blockHex] as any,
}).catch(e => "err: " + e.message?.slice(0, 100));
console.log("swap(USDC,WAVAX,2500000,0):", r1);

// Check what token0/token1 the pool has
const t0 = await client.request({
  method: "eth_call" as any,
  params: [{ to: POOL, data: "0x0dfe1681" }, blockHex] as any, // token0()
}).catch(e => "err");
const t1 = await client.request({
  method: "eth_call" as any,
  params: [{ to: POOL, data: "0xd21220a7" }, blockHex] as any, // token1()
}).catch(e => "err");
console.log("token0:", t0);
console.log("token1:", t1);

// Check getReserves
const reserves = await client.request({
  method: "eth_call" as any,
  params: [{ to: POOL, data: "0x0902f1ac" }, blockHex] as any,
}).catch(e => "err");
console.log("getReserves:", reserves);

// Try calling factory()
const factory = await client.request({
  method: "eth_call" as any,
  params: [{ to: POOL, data: "0xc45a0155" }, blockHex] as any, // factory()
}).catch(e => "err");
console.log("factory:", factory);
