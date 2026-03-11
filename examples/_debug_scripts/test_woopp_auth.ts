import { createPublicClient, http, keccak256, toHex } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const ROUTER = "0x2bef1becdafcfe8990a233d03a98bbb39021c96e";
const LFJ_AGG = "0x63242a4ea82847b20e506b63b0e2e2eff0cc6cb0";
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

// Check if WooPP has a broker/allowlist mechanism
// Common selectors for allowlist checks
const selectors = {
  "brokers(address)": keccak256(toHex("brokers(address)")).slice(0, 10),
  "isAdmin(address)": keccak256(toHex("isAdmin(address)")).slice(0, 10),
  "isBrokerAdded(address)": keccak256(toHex("isBrokerAdded(address)")).slice(0, 10),
};

for (const [name, sel] of Object.entries(selectors)) {
  for (const [addrName, addr] of [["ROUTER", ROUTER], ["LFJ_AGG", LFJ_AGG]]) {
    const data = sel + addr.slice(2).padStart(64, "0");
    const r = await client.request({
      method: "eth_call" as any,
      params: [{ to: WOOPP, data }, blockHex] as any,
    }).catch(e => `err: ${e.message?.slice(0,50)}`);
    console.log(`${name}(${addrName}): ${r}`);
  }
}

// Also try calling WooPP swap directly from router address to see if it allows it
// Build minimal swap calldata  
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const calldata = "0xac8bb7d9" +
  ROUTER.slice(2).padStart(64, "0") +  // broker = router
  "0000000000000000000000000000000000000000000000000000000000000000" +  // direction = 0
  "00000000000000000000000000000000000000000000000000000001270b0180" +  // amount = 4950000000
  "0000000000000000000000000000000000000000000000000000000000000000" +  // minOutput = 0
  "00000000000000000000000000000000000000000000000000000000000000a0" +
  "0000000000000000000000000000000000000000000000000000000000000020" +
  USDC.slice(2).padStart(64, "0");  // data = USDC

// Try calling without state override (will fail on callback)
try {
  const result = await client.request({
    method: "eth_call" as any,
    params: [{ from: ROUTER, to: WOOPP, data: calldata }, blockHex] as any,
  });
  console.log("Direct swap from ROUTER:", result?.slice(0, 66));
} catch (e: any) {
  console.log("Direct swap from ROUTER error:", e.message?.slice(0, 150));
}

// Try with LFJ as from
try {
  const calldata2 = calldata.replace(ROUTER.slice(2).toLowerCase(), LFJ_AGG.slice(2).toLowerCase());
  const result2 = await client.request({
    method: "eth_call" as any,
    params: [{ from: LFJ_AGG, to: WOOPP, data: calldata2 }, blockHex] as any,
  });
  console.log("Direct swap from LFJ:", result2?.slice(0, 66));
} catch (e: any) {
  console.log("Direct swap from LFJ error:", e.message?.slice(0, 150));
}
