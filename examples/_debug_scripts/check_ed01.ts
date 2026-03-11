import { createPublicClient, http, parseAbiItem, formatUnits } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({
  chain: avalanche,
  transport: http("http://localhost:9650/ext/bc/C/rpc"),
});

const txHash = "0xed01f653928b15f2a30d02000b694712f76b879870edb74cb8934281becd60a4" as const;

async function main() {
  const receipt = await client.getTransactionReceipt({ hash: txHash });
  const tx = await client.getTransaction({ hash: txHash });
  
  console.log("=== Transaction ===");
  console.log("From:", tx.from);
  console.log("To:", tx.to);
  console.log("Value:", tx.value.toString());
  console.log("Block:", Number(tx.blockNumber));
  console.log("Status:", receipt.status);
  console.log("Gas used:", Number(receipt.gasUsed));
  console.log("\n=== Logs ===");
  console.log("Total logs:", receipt.logs.length);
  
  // Known event signatures
  const TRANSFER = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef";
  const DEPOSIT = "0xe1fffcc4923d04b559f4d29a8bfc6cda04eb5b0d3c460751c2402c5c5cc9109c"; // Deposit(address,uint256) - WAVAX wrap
  const WITHDRAWAL = "0x7fcf532c15f0a6db0bd6d0e038bea71d30d808c7d98cb3bf7268a95bf5081b65"; // Withdrawal(address,uint256) - WAVAX unwrap
  const SWAP_V2 = "0xd78ad95fa46c994b6551d0da85fc275fe613ce37657fb8d5e3d130840159d822"; // UniV2 Swap
  const SWAP_V3 = "0xc42079f94a6350d7e6235f29174924f928cc2ac818eb64fed8004e115fbcca67"; // UniV3 Swap  
  const LFJ_SWAP = "0xad7d6f97abf51ce18e17a38f4d70e975be9c0708eb987c7c688d5e34c8c8a6ee"; // LFJ V2.1 Swap
  const LFJ_V2_SWAP = "0x4c209b5fc8ad50758f13e2e1088ba56a560dff690a1c6fef26394f4c03821c4f"; // LFJ V2.0
  
  const knownTopics: Record<string, string> = {
    [TRANSFER]: "Transfer",
    [DEPOSIT]: "WAVAX Deposit (wrap)",
    [WITHDRAWAL]: "WAVAX Withdrawal (unwrap)",
    [SWAP_V2]: "UniV2-style Swap",
    [SWAP_V3]: "UniV3-style Swap",
    [LFJ_SWAP]: "LFJ V2.1 Swap",
    [LFJ_V2_SWAP]: "LFJ V2.0 Swap",
  };
  
  for (let i = 0; i < receipt.logs.length; i++) {
    const log = receipt.logs[i];
    const topic0 = log.topics[0] || "none";
    const name = knownTopics[topic0] || `Unknown(${topic0?.slice(0,10)}...)`;
    console.log(`\nLog ${i}: ${name}`);
    console.log("  Address:", log.address);
    console.log("  Topics:", log.topics);
    if (log.data && log.data !== "0x") {
      console.log("  Data:", log.data.slice(0, 130) + (log.data.length > 130 ? "..." : ""));
    }
  }
}

main().catch(console.error);
