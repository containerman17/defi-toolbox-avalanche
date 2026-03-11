import { createPublicClient, http, formatUnits, type Hex, decodeAbiParameters } from "viem";
import { avalanche } from "viem/chains";
import { loadDotEnv } from "../../utils/env.ts";
import { quoteRoute, ROUTER_ADDRESS } from "../../router/index.ts";
import type { StoredPool } from "../../pools/index.ts";
loadDotEnv();

const rpcUrl = process.env.RPC_URL || "https://api.avax.network/ext/bc/C/rpc";
const httpClient = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const USDt = "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7";
const block = 80341389n;
const amount = 10_000_000n;

// V4 pool via on-chain router (uses PoolManager address)
const v4Pool: StoredPool = {
  address: "0x06380C0e0912312B5150364B9DC4542BA0DbBc85",
  poolType: 9, providerName: "uniswap_v4",
  tokens: [USDC, USDt], latestSwapBlock: 0,
  extraData: "id=0xfe74ff9963652d64086e4467e64ceae7847ebf0139cb4046bcbe02c86e48f256,fee=18,ts=1,hooks=0x0000000000000000000000000000000000000000",
};

// V3 pool for comparison
const v3Pool: StoredPool = {
  address: "0x1150403b19315615aad1638d9dd86cd866b2f456",
  poolType: 0, providerName: "uniswap_v3",
  tokens: [USDC, USDt], latestSwapBlock: 0,
};

try {
  const v3Out = await quoteRoute(httpClient, [{ pool: v3Pool, tokenIn: USDC, tokenOut: USDt }], amount, block);
  console.log(`V3 (on-chain router): ${v3Out} (${formatUnits(v3Out, 6)} USDt)`);
} catch (e: any) {
  console.log(`V3 error: ${e.message?.slice(0, 200)}`);
}

try {
  const v4Out = await quoteRoute(httpClient, [{ pool: v4Pool, tokenIn: USDC, tokenOut: USDt }], amount, block);
  console.log(`V4 (on-chain router): ${v4Out} (${formatUnits(v4Out, 6)} USDt)`);
} catch (e: any) {
  console.log(`V4 error: ${e.message?.slice(0, 200)}`);
}
