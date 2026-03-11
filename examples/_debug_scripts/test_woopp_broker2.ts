import { createPublicClient, http, keccak256, type Hex } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });
const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const ROUTER = "0x2bef1becdafcfe8990a233d03a98bbb39021c96e";
const LFJ_AGGREGATOR = "0x63242a4ea82847b20e506b63b0e2e2eff0cc6cb0";
const blockHex = `0x${80108338n.toString(16)}`;

// Try to read storage slots to find broker list
// The brokers mapping might be at different slots
for (let slot = 0; slot < 20; slot++) {
  const key = keccak256(("0x000000000000000000000000" + LFJ_AGGREGATOR.slice(2) + slot.toString(16).padStart(64, '0')) as Hex);
  try {
    const result = await (client.request as any)({
      method: "eth_getStorageAt",
      params: [WOOPP, key, blockHex],
    });
    if (result !== "0x0000000000000000000000000000000000000000000000000000000000000000") {
      console.log(`slot ${slot} for LFJ: ${result}`);
    }
  } catch {}
}

// Check brokerFeeRate for LFJ
const feeRateSig = "brokerFeeRate(address)";
const selector = keccak256(Buffer.from(feeRateSig)).slice(0, 10);
const calldata = selector + "000000000000000000000000" + LFJ_AGGREGATOR.slice(2);
const calldata2 = selector + "000000000000000000000000" + ROUTER.slice(2);

try {
  const r1 = await (client.request as any)({ method: "eth_call", params: [{ to: WOOPP, data: calldata }, blockHex] });
  console.log(`brokerFeeRate(LFJ): ${r1}`);
} catch(e: any) { console.log(`brokerFeeRate(LFJ): revert`); }

try {
  const r2 = await (client.request as any)({ method: "eth_call", params: [{ to: WOOPP, data: calldata2 }, blockHex] });
  console.log(`brokerFeeRate(Router): ${r2}`);
} catch(e: any) { console.log(`brokerFeeRate(Router): revert`); }
