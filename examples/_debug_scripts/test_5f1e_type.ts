import { keccak256, type Hex } from "viem";
import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

// Check what 0xfe029156 is
const sigs = [
  "swap(address,bool,int256,uint160,uint256,address)",
  "swap(address,address,bool,int256,uint160,uint256,address)",
  "swap(address,uint256,uint256,uint160,int256,address,bytes)",
  "swap(address,bool,int256,uint160,address)",
  "swap(address,address,bool,int256,uint160,address,bytes)",
];

for (const sig of sigs) {
  const sel = keccak256(Buffer.from(sig)).slice(0, 10);
  if (sel === "0xfe029156") console.log(`MATCH: ${sig} => ${sel}`);
  else console.log(`${sig} => ${sel}`);
}

// What is this pool?
const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });
const pool = "0x5f1e8ed8468232bab71eda9f4598bda3161f48ea";
const blockHex = `0x${80127543n.toString(16)}`;

// Try to get token0 / token1
const fns = [
  { name: "token0()", data: "0x0dfe1681" },
  { name: "token1()", data: "0xd21220a7" },
  { name: "totalSupply()", data: "0x18160ddd" },
  { name: "fee()", data: "0xddca3f43" },
  { name: "tickSpacing()", data: "0xd0c93a7c" },
];
for (const fn of fns) {
  try {
    const r = await (client.request as any)({ method: "eth_call", params: [{ to: pool, data: fn.data }, blockHex] });
    console.log(`${fn.name}: ${r}`);
  } catch { console.log(`${fn.name}: revert`); }
}
