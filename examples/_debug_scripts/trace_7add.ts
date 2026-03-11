import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

async function main() {
  const receipt = await client.getTransactionReceipt({ hash: "0x7adddb66a5c4000cd7ef7bdcd071063a7b9b5303de7c440bbcc05b93170aff72" as Hex });

  const TOKEN_EXCHANGE_TOPIC = "0xb2e76ae99761dc136e598d4a629bb347eccb9532a5f8bbd72e18467c3c34cc98";
  const TRANSFER_TOPIC = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef";

  console.log("Total logs:", receipt.logs.length);

  for (const log of receipt.logs) {
    if (log.topics[0] === TOKEN_EXCHANGE_TOPIC) {
      console.log("TokenExchange at:", log.address, "data:", log.data, "topics:", log.topics);
    }
  }

  for (const log of receipt.logs) {
    if (log.address.toLowerCase() === "0x204f0620e7e7f07b780535711884835977679bba") {
      console.log("Curve pool log:", log.topics[0], log.data?.slice(0, 66));
    }
  }

  // Check all Transfer events involving the Curve pool
  for (const log of receipt.logs) {
    if (log.topics[0] === TRANSFER_TOPIC) {
      const from = "0x" + log.topics[1]!.slice(26);
      const to = "0x" + log.topics[2]!.slice(26);
      if (from.toLowerCase() === "0x204f0620e7e7f07b780535711884835977679bba" ||
          to.toLowerCase() === "0x204f0620e7e7f07b780535711884835977679bba") {
        console.log("Transfer w/ Curve:", {from: from.slice(0,10), to: to.slice(0,10), token: log.address.slice(0,10), amt: BigInt(log.data!).toString()});
      }
    }
  }
}
main();
