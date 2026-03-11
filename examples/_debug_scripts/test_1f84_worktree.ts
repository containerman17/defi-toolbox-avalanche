// Test WooPP V2 step with worktree bytecode
import { createPublicClient, http, decodeAbiParameters, encodeAbiParameters, keccak256, type Hex } from "viem";
import { avalanche } from "viem/chains";
import { readFileSync } from "node:fs";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const WORKTREE_BYTECODE_PATH = "/home/claude/defi-toolbox-avalanche/.claude/worktrees/agent-ad6808d9/router/contracts/bytecode.hex";
const MAIN_BYTECODE_PATH = "/home/claude/defi-toolbox-avalanche/router/contracts/bytecode.hex";

const ROUTER_ADDRESS = "0x2bef1becdafcfe8990a233d03a98bbb39021c96e";
const DUMMY_SENDER = "0x000000000000000000000000000000000000dEaD";
const WOOPP_V2 = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const USDC = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";
const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const amountIn = 4950000000n;
const blockNumber = BigInt(80108339) - 1n;
const blockHex = `0x${blockNumber.toString(16)}`;

async function testWithBytecode(bytecodeHex: string, label: string) {
  const bytecode = "0x" + readFileSync(bytecodeHex, "utf-8").trim();
  
  // Build calldata for swap(pools, poolTypes, tokens, amountIn, extraDatas)
  // pools = [WOOPP_V2], poolTypes = [14], tokens = [USDC, WAVAX], amountIn, extraDatas = ["0x"]
  const swapAbi = [{
    name: "swap",
    type: "function",
    inputs: [
      { name: "pools", type: "address[]" },
      { name: "poolTypes", type: "uint8[]" },
      { name: "tokens", type: "address[]" },
      { name: "amountIn", type: "uint256" },
      { name: "extraDatas", type: "bytes[]" },
    ],
    outputs: [{ type: "uint256" }],
  }] as const;

  // Manually encode
  const pools = [WOOPP_V2 as `0x${string}`];
  const poolTypes = [14];
  const tokens = [USDC as `0x${string}`, WAVAX as `0x${string}`];
  const extraDatas = ["0x" as `0x${string}`];

  // Encode calldata
  const calldata = "0x" +
    "2e1a7d4d" + // Not right - need actual function selector
    "";
  
  // Use proper ABI encoding via the encodeAbiParameters
  // selector for swap(address[],uint8[],address[],uint256,bytes[])
  const selector = "0x26f69f38"; // will verify
  
  // Actually let's use the exact same approach as hayabusa-router/encode.ts
  // selector: keccak256("swap(address[],uint8[],address[],uint256,bytes[])")[:4]
  
  // Build state override
  const balanceSlot = keccak256(encodeAbiParameters(
    [{type: "address"}, {type: "uint256"}],
    [DUMMY_SENDER as `0x${string}`, 9n]
  ));
  const allowanceSlot = keccak256(encodeAbiParameters(
    [{type: "address"}, {type: "uint256"}],
    [DUMMY_SENDER as `0x${string}`, keccak256(encodeAbiParameters(
      [{type: "address"}, {type: "uint256"}],
      [ROUTER_ADDRESS as `0x${string}`, 9n]
    )) as any]
  ));

  const stateOverride: Record<string, any> = {
    [ROUTER_ADDRESS]: { code: bytecode as Hex },
    [USDC]: {
      stateDiff: {
        [balanceSlot]: `0x${amountIn.toString(16).padStart(64, "0")}` as Hex,
      }
    },
    [DUMMY_SENDER]: {},
  };

  // Call quoteRoute via the deployed router logic
  // selector from viem: encodeFunctionData
  const { encodeFunctionData } = await import("viem");
  const data = encodeFunctionData({
    abi: swapAbi,
    functionName: "swap",
    args: [pools, poolTypes, tokens, amountIn, extraDatas],
  });

  try {
    const result = await client.request({
      method: "eth_call" as any,
      params: [
        { from: DUMMY_SENDER, to: ROUTER_ADDRESS, data },
        blockHex,
        stateOverride,
      ] as any,
    });
    const [out] = decodeAbiParameters([{type:"uint256"}], result as `0x${string}`);
    console.log(`[${label}] WooPP V2 output: ${out}`);
  } catch (err: any) {
    console.log(`[${label}] Error: ${err.message?.slice(0, 200)}`);
  }
}

await testWithBytecode(WORKTREE_BYTECODE_PATH, "worktree");
await testWithBytecode(MAIN_BYTECODE_PATH, "main");
