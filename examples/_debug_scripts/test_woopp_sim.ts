import { createPublicClient, http, parseAbi } from "viem";
import { quoteRoute } from "../router/quote.ts";

const client = createPublicClient({ 
  transport: http("http://localhost:9650/ext/bc/C/rpc") 
});

async function main() {
  // Test single-hop WooPP: 0x152b -> WETH.e
  const payload1hop = {
    block: 80095244,
    inputToken: "0x152b9d0fdc40c096757f570a51e494bd4b943e50",
    outputToken: "0x49d5c2bdffac6ce2bfdb6640f4f80f226bc10bab", // WETH.e
    amountIn: "57",
    pools: ["0x4c4af8dbc524681930a27b2f1af5bcc8062e6fb7"],
    poolTypes: [5],
    tokens: [
      "0x152b9d0fdc40c096757f570a51e494bd4b943e50",
      "0x49d5c2bdffac6ce2bfdb6640f4f80f226bc10bab",
    ],
    extraDatas: [""],
  };
  
  const result = await quoteRoute(payload1hop as any, client);
  console.log("WooPP 0x152b->WETH.e simulation result:", result?.toString());
  console.log("Expected from trace:", "19618190216835");
}
main().catch(console.error);
