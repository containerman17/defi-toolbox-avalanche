import { createPublicClient, http, encodeFunctionData, decodeAbiParameters, type Hex } from "viem";
import { avalanche } from "viem/chains";
import * as fs from "fs";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });
const bc = fs.readFileSync("/home/claude/defi-toolbox-avalanche/quoting/data/quoter_bytecode.hex", "utf-8").trim();
const quoterBytecode = (bc.startsWith("0x") ? bc : `0x${bc}`) as Hex;
const QUOTER_ADDR = "0x00000000000000000000000000000000DeaDBeef" as Hex;
const DUMMY = "0x000000000000000000000000000000000000dEaD" as Hex;

const abi = [{ name: "swap", type: "function", inputs: [{name:"pools",type:"address[]"},{name:"poolTypes",type:"uint8[]"},{name:"tokens",type:"address[]"},{name:"amountIn",type:"uint256"}], outputs: [{type:"uint256"}] }] as const;

const calldata = encodeFunctionData({ abi, functionName: "swap", args: [
  ["0xfAe3f424a0a47706811521E3ee268f00cFb5c45E"],
  [0],
  ["0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7", "0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E"],
  1000000000000000000n,
]});

// Give the quoter contract WAVAX balance via known storage slot
// WAVAX balance slot for holder: keccak256(abi.encode(holder, 3))  (slot 3 for WAVAX)
import { keccak256, pad, toHex, maxUint256 } from "viem";
const wavax = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const holderPadded = pad(QUOTER_ADDR.toLowerCase() as Hex, { size: 32 });
const slotPadded = pad(toHex(3), { size: 32 });
const balSlot = keccak256(`${holderPadded}${slotPadded.slice(2)}` as Hex);
const balValue = pad(toHex(1000000000000000000000n), { size: 32 });

const stateOverride: any[] = [
  { address: QUOTER_ADDR, code: quoterBytecode },
  { address: wavax as Hex, stateDiff: [{ slot: balSlot, value: balValue }] },
];

try {
  const result = await client.call({ account: DUMMY, to: QUOTER_ADDR, data: calldata, stateOverride, blockNumber: 80390594n });
  console.log("raw:", result.data);
  if (result.data && result.data !== "0x") {
    const [out] = decodeAbiParameters([{type:"uint256"}], result.data);
    console.log("amountOut:", out.toString());
  }
} catch(e: any) {
  console.error("ERROR:", e.shortMessage || e.message);
}
