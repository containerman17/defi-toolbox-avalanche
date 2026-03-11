// Check 0x447a7f6a tx receipt to find transfer logs
import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const txHash = "0x447a7f6aca1d7593abec732829422fb0ab3bce338f6b628945cb0f02f6f7cc2c";

const receipt = await client.getTransactionReceipt({ hash: txHash as `0x${string}` });

// Transfer topic
const TRANSFER_TOPIC = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef";

// Known tokens
const tokens: Record<string, string> = {
  "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e": "USDC",
  "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664": "USDC.e",
  "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7": "USDt",
  "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7": "WAVAX",
};

for (const log of receipt.logs) {
  if (log.topics[0] === TRANSFER_TOPIC && log.topics.length === 3) {
    const token = tokens[log.address.toLowerCase()] ?? log.address.slice(0, 10);
    const from = "0x" + log.topics[1].slice(26);
    const to = "0x" + log.topics[2].slice(26);
    const amount = BigInt(log.data);
    if (token === "USDC.e" || token === "USDt") {
      console.log(`${token}: ${from} → ${to} amount=${amount}`);
    }
  }
}
