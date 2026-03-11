// Final test: check what eth_call returns for WooPP swap with our params
import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { readFileSync } from "node:fs";
import { keccak256, encodeAbiParameters, decodeAbiParameters } from "viem";
import { ROUTER_ADDRESS } from "hayabusa-router";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const WOOPP = "0xABa7eD514217D51630053d73D358aC2502d3f9BB";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const amountIn = 4950000000n;
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

const bytecode = "0x" + readFileSync("/home/claude/defi-toolbox-avalanche/router/contracts/bytecode.hex", "utf-8").trim();
const slot = keccak256(encodeAbiParameters([{type: "address"}, {type: "uint256"}], [ROUTER_ADDRESS as Hex, 9n]));

const stateOverride = {
  [ROUTER_ADDRESS]: { code: bytecode as Hex },
  [USDC]: { stateDiff: { [slot]: `0x${amountIn.toString(16).padStart(64, "0")}` as Hex } },
};

// The `_swapWooPPV2` call in our router sends minOutput=0
// Let's add some debugging by calling a test that shows what WooPP returns

// First, let me check: does WooPP actually compute a non-zero amount?
// The issue might be that WooPP computes the USDC to pull via `transferFrom`, 
// NOT via the callback. Let me re-read the real trace:

// Real trace: WooPP calls CALLBACK (c3251075) which does a transfer
// But also: does WooPP also try transferFrom?

// From bytecode: we saw "RktransferFrom" string
// WooPP might check if broker already sent tokens via balanceOf check,
// OR it uses transferFrom to pull tokens

// The key: in real tx, WooPP first transfers WAVAX to broker (CALL 0xa9059cbb),
// then calls callback. 

// What if WooPP V2 uses msg.sender for the fromAmount check?
// I.e., WooPP checks how much of the input token it has vs what it expected

// Let me try: what if the issue is that WooPP tries to do transferFrom
// on our router to pull USDC, and that fails because our router doesn't have
// the right approve logic?

// Try calling with transferFrom approve set up
const allowanceSlot = keccak256(encodeAbiParameters(
  [{type:"address"},{type:"uint256"}],
  [ROUTER_ADDRESS as Hex, keccak256(encodeAbiParameters(
    [{type:"address"},{type:"uint256"}],
    [WOOPP as Hex, 9n]
  )) as any]
));
console.log("This is getting complex. Let me check the actual eth_call trace for the quoteRoute call.");

// Try to add allowance for WooPP to pull USDC from ROUTER
// In USDC storage: allowance[router][woopp] needs to be set
// The mapping is usually: keccak256(woopp . keccak256(router . slot_idx))
// But USDC uses slot 10 for allowances
const innerKey = keccak256(encodeAbiParameters([{type:"address"},{type:"uint256"}], [WOOPP as Hex, 9n]));
const outerSlot = keccak256(encodeAbiParameters([{type:"address"},{type:"bytes32"}], [ROUTER_ADDRESS as Hex, innerKey as `0x${string}`]));
console.log("Allowance slot:", outerSlot);

// Try with allowance set (just in case WooPP uses transferFrom)
const stateOverrideWithAllowance = {
  ...stateOverride,
  [USDC]: { stateDiff: { 
    [slot]: `0x${amountIn.toString(16).padStart(64, "0")}` as Hex,
    // Also try setting allowance 
  }},
};

// Actually, looking at this more carefully:
// The standard USDC has allowances at a different mapping structure
// Let me just try raw eth_call with structlog to see what happens at each SLOAD

// Better: let me instrument by calling oracle then WooPP manually to trace at the Solidity level

// APPROACH: try calling _executeSingle directly on our router
// which is an external function we can call
const executeSingleCalldata = 
  "0xbbf6e671" +  // _executeSingle(address,uint8,address,address,uint256,bytes)
  WOOPP.slice(2).padStart(64, "0") +  // pool
  "000000000000000000000000000000000000000000000000000000000000000e" +  // poolType = 14
  USDC.slice(2).padStart(64, "0") +  // tokenIn
  WAVAX.slice(2).padStart(64, "0") +  // tokenOut
  amountIn.toString(16).padStart(64, "0") +  // amountIn
  "00000000000000000000000000000000000000000000000000000000000000c0" +  // extraData offset
  "0000000000000000000000000000000000000000000000000000000000000000";  // extraData length = 0

console.log("_executeSingle calldata (first bytes):", executeSingleCalldata.slice(0, 10));

const result = await client.request({
  method: "eth_call" as any,
  params: [
    { from: "0x000000000000000000000000000000000000dEaD", to: ROUTER_ADDRESS, data: executeSingleCalldata },
    blockHex,
    stateOverride,
  ] as any,
}).catch(e => "err: " + e.message?.slice(0, 300));
console.log("_executeSingle result:", result);
