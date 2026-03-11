// Test individual steps of 0x1f84c569 to find which one produces wrong output
import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";
import { quoteRoute } from "hayabusa-router";

const payload = {
  block: 80108339,
  steps: [
    {
      amountIn: "4950000000",
      pools: ["0xaba7ed514217d51630053d73d358ac2502d3f9bb"],
      poolTypes: [14],
      tokens: ["0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e", "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7"],
      extraDatas: [""],
      route: "woopp_v2(USDC→WAVAX)",
    },
    {
      amountIn: "19250000000",
      pools: ["0xf01449c0ba930b6e2caca3def3ccbd7a3e589534"],
      poolTypes: [0],
      tokens: ["0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e", "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7"],
      extraDatas: [""],
      route: "pharaoh_v3(USDC→WAVAX)",
    },
    {
      amountIn: "11000000000",
      pools: ["0xa02ec3ba8d17887567672b2cdcaf525534636ea0"],
      poolTypes: [1],
      tokens: ["0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e", "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7"],
      extraDatas: [""],
      route: "algebra(USDC→WAVAX)",
    },
    {
      amountIn: "16500000000",
      pools: ["0x1150403b19315615aad1638d9dd86cd866b2f456"],
      poolTypes: [0],
      tokens: ["0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e", "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7"],
      extraDatas: [""],
      route: "uniswap_v3(USDC→USDt)",
    },
    {
      amountIn: "2750000000",
      pools: ["0x41100c6d2c6920b10d12cd8d59c8a9aa2ef56fc7"],
      poolTypes: [1],
      tokens: ["0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e", "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7"],
      extraDatas: [""],
      route: "algebra(USDC→WAVAX)",
    },
    {
      amountIn: "550000000",
      pools: ["0x668aa7aefa8512416fc6244afbe5129200277a69"],
      poolTypes: [1],
      tokens: ["0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e", "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7"],
      extraDatas: [""],
      route: "algebra(USDC→WAVAX)",
    },
    {
      amountIn: "16497584184",
      pools: ["0x4c4af8dbc524681930a27b2f1af5bcc8062e6fb7"],
      poolTypes: [5],
      tokens: ["0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7", "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7"],
      extraDatas: [""],
      route: "woofi_v2(USDt→WAVAX)",
    },
  ]
};

async function main() {
  const client = createPublicClient({
    chain: avalanche,
    transport: http(process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc"),
  });

  const blockNumber = BigInt(payload.block) - 1n;
  let totalOut = 0n;

  for (const step of payload.steps) {
    const route = step.pools.map((pool, i) => ({
      pool: {
        address: pool,
        providerName: "",
        poolType: step.poolTypes[i],
        tokens: [step.tokens[i], step.tokens[i + 1]],
        latestSwapBlock: 0,
        extraData: step.extraDatas[i] || undefined,
      },
      tokenIn: step.tokens[i],
      tokenOut: step.tokens[i + 1],
    }));

    try {
      const out = await quoteRoute(client, route, BigInt(step.amountIn), blockNumber);
      const isWAVAX = step.tokens[step.tokens.length - 1] === "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
      if (isWAVAX) totalOut += out;
      console.log(`${step.route}: in=${step.amountIn} out=${out} (${isWAVAX ? "counted" : "intermediate"})`);
    } catch (err) {
      console.log(`${step.route}: REVERT - ${err.message?.slice(0, 150)}`);
    }
  }

  const expectedOut = 5787588334655160191401n;
  const delta = totalOut - expectedOut;
  const pct = Number(delta * 10000n / expectedOut) / 100;
  console.log(`\nTotal: ${totalOut}`);
  console.log(`Expected: ${expectedOut}`);
  console.log(`Delta: ${pct.toFixed(3)}%`);
}

main().catch(console.error);
