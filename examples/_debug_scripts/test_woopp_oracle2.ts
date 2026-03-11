import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const ORACLE = "0xd92e3c8f1c5e835e4e76173c4e83bf517f61b737";
const FEED = "0x5ecf662abb8c2ab099862f9ef2ddc16cbc8a9977";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const block = BigInt(80108339) - 1n;
const blockHex = `0x${block.toString(16)}`;

// Call oracle with overrides that simulate our router setup
// The question is: does oracle.c1701b67 work correctly in an eth_call?
const result1 = await client.request({
  method: "eth_call" as any,
  params: [
    { to: ORACLE, data: "0xc1701b67" + WAVAX.slice(2).padStart(64, "0") },
    blockHex,
  ] as any,
}).catch(e => "err");
console.log("oracle.c1701b67(WAVAX) at block:", result1);

// Check if state overrides affect the oracle result
const result2 = await client.request({
  method: "eth_call" as any,
  params: [
    { to: ORACLE, data: "0xc1701b67" + WAVAX.slice(2).padStart(64, "0") },
    blockHex,
    { "0x0000000000000000000000000000000000000000": {} },  // empty override
  ] as any,
}).catch(e => "err");
console.log("oracle.c1701b67(WAVAX) with empty override:", result2);

// Check WooPP.swap with state overrides that include router bytecode
// but DON'T include our modified router code - just see if pure WooPP swap works
const ROUTER = "0x2bef1becdafcfe8990a233d03a98bbb39021c96e";
const { keccak256, encodeAbiParameters } = await import("viem");
const slot = keccak256(encodeAbiParameters([{type: "address"}, {type: "uint256"}], [ROUTER as `0x${string}`, 9n]));

// Simulate: router has USDC, calls WooPP directly
// WooPP should give WAVAX out
const swapCalldata = "0xac8bb7d9" + 
  // broker = router
  ROUTER.slice(2).padStart(64, "0") +
  // direction = 0 (sell USDC, get WAVAX)
  "0".padStart(64, "0") +
  // amount = 4950000000
  (4950000000n).toString(16).padStart(64, "0") +
  // minOutput = 0
  "0".padStart(64, "0") +
  // bytes offset
  "00000000000000000000000000000000000000000000000000000000000000a0" +
  // bytes length = 32
  "0000000000000000000000000000000000000000000000000000000000000020" +
  // base token = WAVAX
  WAVAX.slice(2).padStart(64, "0");

// For this to work, the router needs USDC and a callback
// Let's just check if the oracle read works
const swapResult = await client.request({
  method: "eth_call" as any,
  params: [
    { from: ROUTER, to: "0xaba7ed514217d51630053d73d358ac2502d3f9bb", data: swapCalldata },
    blockHex,
    { 
      [USDC]: { stateDiff: { [slot]: `0x${(4950000000n).toString(16).padStart(64, "0")}` } },
    },
  ] as any,
}).catch(e => "err: " + e.message?.slice(0, 150));
console.log("\nWooPP swap from router:", swapResult);
