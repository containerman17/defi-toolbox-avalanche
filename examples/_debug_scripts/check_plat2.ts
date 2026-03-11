import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const RPC_URL = process.env.RPC_URL || "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(RPC_URL) });

async function callMethod(addr: string, method: string, label: string) {
  try {
    const data = await (client as any).request({
      method: "eth_call",
      params: [{
        to: addr,
        data: method
      }, "latest"]
    });
    return data;
  } catch(e: any) {
    return `Error: ${e.message?.slice(0, 80)}`;
  }
}

async function main() {
  const POOL1 = "0xed2a7edd7413021d440b09d654f3b87712abab66";
  const POOL2 = "0xa196a03653f6cc5ca0282a8bd7ec60e93f620afc";
  
  // Platypus pool methods to check:
  // getTokenAddresses() - 0x84a42045 (but wait that's the implementation)
  // asset(address) - look at source
  // getTokens() - check
  
  // Let's try to get code for these
  const code1 = await (client as any).request({
    method: "eth_getCode",
    params: [POOL1, "latest"]
  });
  const code2 = await (client as any).request({
    method: "eth_getCode",
    params: [POOL2, "latest"]
  });
  
  console.log(`POOL1 code length: ${code1.length}`);
  console.log(`POOL2 code length: ${code2.length}`);
  
  // Check what the delegatecall target 0x84a42045 is
  const impl = "0x84a420454d06cf89d3dfcbd69f5a0b3e6d45cef3";
  const codeImpl = await (client as any).request({
    method: "eth_getCode",
    params: [impl, "latest"]
  });
  console.log(`Impl code length: ${codeImpl.length}`);
  
  // Try calling swap methods - getTokens
  // Let's try to call the platypus main router at 0x66357dcace80431aee0a75f62907408cf510ebcc
  // and see if it can route through these pools
  
  // Actually let me check if these pool addresses exist in the platypus protocol
  // Try token() on POOL1
  const token1 = await callMethod(POOL1, "0xfc0c546a", "token()");
  console.log("POOL1 token():", token1);
  
  // Try getTokens() on POOL1
  const tokens1 = await callMethod(POOL1, "0x84a42045", "getTokenAddresses()");
  console.log("POOL1 getTokenAddresses():", tokens1);
  
  // Let me look at what selector 0x91695586 is (used in the original tx)
  // This is likely a swap function
  console.log("\nSelector 0x91695586 is the swap function called");
  
  // Let me check what token 0xcfc37a6a...is by calling totalSupply, name, symbol
  const cfc = "0xcfc37a6ab183dd2adc73cd5763be8a52b3e2b35a";
  
  const cfcCode = await (client as any).request({
    method: "eth_getCode",
    params: [cfc, "latest"]
  });
  console.log(`\n0xcfc37a6a code length: ${cfcCode.length}`);
  
  // Try ERC20 methods
  const cfcSymbol = await callMethod(cfc, "0x95d89b41", "symbol()");
  console.log("0xcfc37a6a symbol():", cfcSymbol);
  
  const cfcName = await callMethod(cfc, "0x06fdde03", "name()");
  console.log("0xcfc37a6a name():", cfcName);
}

main().catch(console.error);
