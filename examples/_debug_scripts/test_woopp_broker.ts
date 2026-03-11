import { createPublicClient, http, keccak256, type Hex } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });
const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const ROUTER = "0x2bef1becdafcfe8990a233d03a98bbb39021c96e";
const LFJ_AGGREGATOR = "0x63242a4ea82847b20e506b63b0e2e2eff0cc6cb0"; // from the trace
const blockHex = `0x${80108338n.toString(16)}`;

const sigs = [
  "isBrokerAdded(address)",
  "isValidBroker(address)",
  "brokers(address)",
  "isAdmin(address)",
];

for (const sig of sigs) {
  const selector = keccak256(Buffer.from(sig)).slice(0, 10);
  const calldata = selector + "000000000000000000000000" + ROUTER.slice(2);
  try {
    const result = await (client.request as any)({
      method: "eth_call",
      params: [{ to: WOOPP, data: calldata }, blockHex],
    });
    console.log(`${sig}: ${result}`);
  } catch (e: any) {
    // Try with LFJ_AGGREGATOR
    const calldata2 = selector + "000000000000000000000000" + LFJ_AGGREGATOR.slice(2);
    try {
      const result2 = await (client.request as any)({
        method: "eth_call",
        params: [{ to: WOOPP, data: calldata2 }, blockHex],
      });
      console.log(`${sig}(LFJ): ${result2}`);
    } catch (e2: any) {
      console.log(`${sig}: revert`);
    }
  }
}
