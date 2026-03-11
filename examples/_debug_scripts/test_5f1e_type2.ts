import { keccak256 } from "viem";
import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

// Check more selectors for 0xfe029156
const sigs = [
  "swap(address,uint256,address,bytes)",
  "swap(address,uint256,uint256,address,bytes)", 
  "swap(address,bool,int256,uint160,bytes)",
  "swap(uint256,uint256,bytes)",
  "swap(address,uint256,address)",
  "swap(address,address,uint256,uint256,address,bytes)",
  "onSwap(address,bool,int256,uint160,bytes)",
  "swapWithFee(address,bool,int256,uint160,bytes,address)",
];

for (const sig of sigs) {
  const sel = keccak256(Buffer.from(sig)).slice(0, 10);
  if (sel === "0xfe029156") console.log(`MATCH: ${sig} => ${sel}`);
}

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });
const pool = "0x5f1e8ed8468232bab71eda9f4598bda3161f48ea";
const blockHex = `0x${80127543n.toString(16)}`;

// Try more functions
const fns = [
  { name: "getReserves()", data: "0x0902f1ac" },
  { name: "factory()", data: "0xc45a0155" },
  { name: "slot0()", data: "0x3850c7bd" },
  { name: "liquidity()", data: "0x1a686502" },
  { name: "globalState()", data: "0xe76c01e4" },
  { name: "quoteTokenAddress()", data: "0xdc16faf4" },
  { name: "BASE()", data: "0x8d928af8" },
  { name: "baseToken()", data: "0xc55dae63" },
  { name: "quoteToken()", data: "0xdc16faf4" },
  { name: "tokenA()", data: "0xfcf5a168" },
  { name: "tokenB()", data: "0x00d5cec5" },
];

for (const fn of fns) {
  try {
    const r = await (client.request as any)({ method: "eth_call", params: [{ to: pool, data: fn.data }, blockHex] });
    if (r !== "0x" && r !== "0x" + "0".repeat(64)) console.log(`${fn.name}: ${r}`);
  } catch { }
}
