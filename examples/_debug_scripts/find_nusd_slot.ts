import { createPublicClient, http, keccak256, encodePacked, pad, toHex, encodeAbiParameters } from "viem";
import { avalanche } from "viem/chains";

const RPC = "http://localhost:9650/ext/bc/C/rpc";
const NUSD = "0xcfc37a6ab183dd4aed08c204d1c2773c0b1bdf46" as const;
const POOL = "0xed2a7edd7413021d440b09d654f3b87712abab66" as const;

const client = createPublicClient({
  chain: avalanche,
  transport: http(RPC),
});

async function main() {
  // Get actual balance
  const balance = await client.readContract({
    address: NUSD,
    abi: [{ type: "function", name: "balanceOf", inputs: [{ type: "address", name: "" }], outputs: [{ type: "uint256" }], stateMutability: "view" }],
    functionName: "balanceOf",
    args: [POOL],
  });
  console.log(`balanceOf(${POOL}) = ${balance}`);

  if (balance === 0n) {
    console.log("Balance is 0, cannot determine slot");
    return;
  }

  const slots = [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 51, 52, 100, 101, 102, 103, 104, 105];

  // Standard Solidity: keccak256(abi.encode(address, slot))
  console.log("\n--- Solidity ordering: keccak256(abi.encode(address, slot)) ---");
  for (const slot of slots) {
    const key = keccak256(
      encodeAbiParameters(
        [{ type: "address" }, { type: "uint256" }],
        [POOL, BigInt(slot)]
      )
    );

    const value = await client.request({
      method: "eth_getStorageAt" as any,
      params: [NUSD, key, "latest"] as any,
    });

    const storedValue = BigInt(value as string);
    if (storedValue !== 0n) {
      console.log(`Slot ${slot}: ${storedValue}${storedValue === balance ? " *** MATCH ***" : ""}`);
    }
  }

  // Vyper ordering: keccak256(abi.encode(slot, address))
  console.log("\n--- Vyper ordering: keccak256(abi.encode(slot, address)) ---");
  for (const slot of slots) {
    const key = keccak256(
      encodeAbiParameters(
        [{ type: "uint256" }, { type: "address" }],
        [BigInt(slot), POOL]
      )
    );

    const value = await client.request({
      method: "eth_getStorageAt" as any,
      params: [NUSD, key, "latest"] as any,
    });

    const storedValue = BigInt(value as string);
    if (storedValue !== 0n) {
      console.log(`Slot ${slot}: ${storedValue}${storedValue === balance ? " *** MATCH ***" : ""}`);
    }
  }
}

main().catch(console.error);
