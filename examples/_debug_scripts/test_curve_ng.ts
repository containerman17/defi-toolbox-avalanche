import { createPublicClient, http, type Hex, encodeFunctionData, decodeFunctionResult } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const CURVE_POOL = "0x204f0620e7e7f07b780535711884835977679bba";
const TOKEN_IN = "0x152b9d0fdc40c096757f570a51e494bd4b943e50"; // BTC.b
const TOKEN_OUT = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7"; // WAVAX

async function main() {
  // First check get_dy to see if the pool works
  const getDyAbi = [{
    name: "get_dy",
    type: "function" as const,
    inputs: [
      { name: "i", type: "uint256" },
      { name: "j", type: "uint256" },
      { name: "dx", type: "uint256" },
    ],
    outputs: [{ type: "uint256" }],
    stateMutability: "view" as const,
  }];

  // Try get_dy at a recent block
  try {
    const result = await client.readContract({
      address: CURVE_POOL as Hex,
      abi: getDyAbi,
      functionName: "get_dy",
      args: [1n, 2n, 350n],
      blockNumber: 80111390n, // block before the target tx
    });
    console.log("get_dy(1, 2, 350) =", result);
  } catch (e: any) {
    console.log("get_dy error:", e.message?.slice(0, 200));
  }

  // Check coins
  const coinsAbi = [{
    name: "coins",
    type: "function" as const,
    inputs: [{ name: "i", type: "uint256" }],
    outputs: [{ type: "address" }],
    stateMutability: "view" as const,
  }];

  for (let i = 0; i < 3; i++) {
    try {
      const addr = await client.readContract({
        address: CURVE_POOL as Hex,
        abi: coinsAbi,
        functionName: "coins",
        args: [BigInt(i)],
      });
      console.log(`coins(${i}) = ${addr}`);
    } catch (e: any) {
      console.log(`coins(${i}) error: ${e.message?.slice(0, 100)}`);
    }
  }

  // Check if exchange uses payable (AVAX output might need receiver=payable)
  // Check the actual exchange function signature by looking at the ABI
  const exchangeAbi = [{
    name: "exchange",
    type: "function" as const,
    inputs: [
      { name: "i", type: "uint256" },
      { name: "j", type: "uint256" },
      { name: "dx", type: "uint256" },
      { name: "min_dy", type: "uint256" },
    ],
    outputs: [{ type: "uint256" }],
    stateMutability: "payable" as const,
  }];

  // Try exchange with state overrides
  const exchangeData = encodeFunctionData({
    abi: exchangeAbi,
    functionName: "exchange",
    args: [1n, 2n, 350n, 0n],
  });
  console.log("exchange selector:", exchangeData.slice(0, 10));
  console.log("Expected: 0x5b41b908");

  // Check if the exchange function uses WETH or native AVAX for WAVAX output
  // The Curve tricrypto pool might wrap/unwrap native AVAX
  // If coins(2) is WAVAX (the wrapped version), it should use ERC20 transfers
}

main().catch(console.error);
