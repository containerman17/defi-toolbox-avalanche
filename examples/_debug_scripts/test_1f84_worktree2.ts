// Test WooPP V2 step with worktree bytecode directly
import { createPublicClient, http, decodeAbiParameters, encodeAbiParameters, keccak256, encodeFunctionData, type Hex } from "viem";
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

// USDC balance slot from token_overrides.json — slot 9 for standard ERC20 mapping
function getUSDCBalanceSlot(holder: string): Hex {
  const k = keccak256(
    ("0x" + holder.toLowerCase().slice(2).padStart(64, "0") + "0000000000000000000000000000000000000000000000000000000000000009") as Hex
  );
  return k;
}

function getUSDCAllowanceSlot(owner: string, spender: string): Hex {
  // allowances[owner][spender] = mapping(address => mapping(address => uint256))
  // slot for allowances mapping (slot 10 for USDC)
  const innerKey = keccak256(
    ("0x" + owner.toLowerCase().slice(2).padStart(64, "0") + "000000000000000000000000000000000000000000000000000000000000000a") as Hex
  );
  return keccak256(
    ("0x" + spender.toLowerCase().slice(2).padStart(64, "0") + innerKey.slice(2)) as Hex
  );
}

async function testWithBytecodeFile(bytecodeFile: string, label: string) {
  const bytecode = "0x" + readFileSync(bytecodeFile, "utf-8").trim();
  
  const balSlot = getUSDCBalanceSlot(DUMMY_SENDER);
  const allowSlot = getUSDCAllowanceSlot(DUMMY_SENDER, ROUTER_ADDRESS);
  
  const stateOverride: Record<string, any> = {
    [ROUTER_ADDRESS]: { code: bytecode as Hex },
    [USDC]: {
      stateDiff: {
        [balSlot]: `0x${amountIn.toString(16).padStart(64, "0")}` as Hex,
        [allowSlot]: `0x${"f".repeat(64)}` as Hex,
      }
    },
  };

  const data = encodeFunctionData({
    abi: swapAbi,
    functionName: "swap",
    args: [[WOOPP_V2 as `0x${string}`], [14], [USDC as `0x${string}`, WAVAX as `0x${string}`], amountIn, ["0x"]],
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
    if (result === "0x" || !result) {
      console.log(`[${label}] Empty response`);
    } else {
      const [out] = decodeAbiParameters([{type:"uint256"}], result as `0x${string}`);
      console.log(`[${label}] WooPP V2 output: ${out}`);
    }
  } catch (err: any) {
    console.log(`[${label}] Error: ${err.message?.slice(0, 300)}`);
  }
}

await testWithBytecodeFile(WORKTREE_BYTECODE_PATH, "worktree");
await testWithBytecodeFile(MAIN_BYTECODE_PATH, "main");
