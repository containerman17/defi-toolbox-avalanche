import { createPublicClient, http, keccak256, type Hex } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });
const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const LFJ_AGGREGATOR = "0x63242a4ea82847b20e506b63b0e2e2eff0cc6cb0";
const blockHex = `0x${80108338n.toString(16)}`;

// Try direct slot reads for LFJ as broker mapping
// WooFi brokers mapping is usually at slot 5 or 6
for (let slot = 0; slot < 30; slot++) {
  const addrPadded = ("0x000000000000000000000000" + LFJ_AGGREGATOR.slice(2)) as Hex;
  const slotHex = ("0x" + slot.toString(16).padStart(64, '0')) as Hex;
  const combined = (addrPadded + slotHex.slice(2)) as Hex;
  const key = keccak256(combined);
  try {
    const result = await (client.request as any)({
      method: "eth_getStorageAt",
      params: [WOOPP, key, blockHex],
    });
    if (result !== "0x0000000000000000000000000000000000000000000000000000000000000000") {
      console.log(`keccak(LFJ||slot${slot}): ${result}`);
    }
  } catch {}
}
console.log("Done checking broker slots");
