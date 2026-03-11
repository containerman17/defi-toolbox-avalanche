import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const RPC_URL = process.env.RPC_URL || "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(RPC_URL) });

async function main() {
  const POOL1 = "0xed2a7edd7413021d440b09d654f3b87712abab66"; // USDC.e in
  const POOL2 = "0xa196a03653f6cc5ca0282a8bd7ec60e93f620afc"; // USDt out
  
  // Check storage slots to understand contract type
  // Platypus pool implementation is 0x84a420454d06cf89d3dfcbd69f5a0b3e6d45cef3
  // Let's check the EIP-1967 proxy implementation slot
  const IMPL_SLOT = "0x360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc";
  
  const impl1 = await (client as any).request({
    method: "eth_getStorageAt",
    params: [POOL1, IMPL_SLOT, "latest"]
  });
  const impl2 = await (client as any).request({
    method: "eth_getStorageAt",
    params: [POOL2, IMPL_SLOT, "latest"]
  });
  
  console.log("POOL1 impl slot:", impl1);
  console.log("POOL2 impl slot:", impl2);
  
  // Try Platypus asset token mapping
  // asset(address token) → returns asset address
  // Function selector: 0x1d1b8e0d is asset(address)
  
  // Try getTokenAddresses() → 0x84a42045
  // Actually let me look at what methods these contracts have
  
  // The delegatecall target seems to be 0x84a42045... Let's look at it 
  // Check from the code at POOL1, maybe it's an OpenZeppelin transparent proxy
  // slot 0x b53127684a568b3173ae13b9f8a6016e243e63b6e8ee1178d6a717850b5d6103
  const ADMIN_SLOT = "0xb53127684a568b3173ae13b9f8a6016e243e63b6e8ee1178d6a717850b5d6103";
  const admin1 = await (client as any).request({
    method: "eth_getStorageAt",
    params: [POOL1, ADMIN_SLOT, "latest"]
  });
  console.log("\nPOOL1 admin slot:", admin1);
  
  // Check slot 0 - often token address
  const slot0 = await (client as any).request({
    method: "eth_getStorageAt",
    params: [POOL1, "0x0", "latest"]
  });
  console.log("POOL1 slot 0:", slot0);
  
  // Check what kind of swap this is - call swap with USDC.e
  // 0x91695586 is the swap function selector in the original tx
  
  // Let's try to decode by calling the function with ABI-encoded args
  // First arg USDC.e (token), amount, min, to
  const USDC_E = "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664";
  const USDt = "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7";
  
  // Try calling with eth_call to see what happens (should fail with useful info)
  // Arg encoding: (address fromToken, uint256 fromAmount, uint256 minimumToAmount, address to, uint256 deadline)
  // or maybe (address fromToken, address toToken, uint256 fromAmount, uint256 minimumToAmount, address to, uint256 deadline)
  
  // Let's decode the input from the trace
  // We know from trace that call to 0xed2a7edd had input beginning 0x91695586
  // Let me get the actual input data
  const trace = await (client as any).request({
    method: "debug_traceTransaction",
    params: ["0x447a7f6aca1d7593abec732829422fb0ab3bce338f6b628945cb0f02f6f7cc2c", 
             { tracer: "callTracer", tracerConfig: { withLog: false } }]
  });
  
  function findCallTo(call: any, target: string): any | null {
    if (call.to?.toLowerCase() === target.toLowerCase()) return call;
    for (const sub of (call.calls || [])) {
      const found = findCallTo(sub, target);
      if (found) return found;
    }
    return null;
  }
  
  const pool1Call = findCallTo(trace, POOL1);
  const pool2Call = findCallTo(trace, POOL2);
  
  if (pool1Call) console.log("\nPOOL1 call input:", pool1Call.input);
  if (pool2Call) console.log("POOL2 call input:", pool2Call.input);
}

main().catch(console.error);
