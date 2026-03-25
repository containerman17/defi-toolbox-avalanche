// probe_pharaoh_v1.ts — Probe missing Pharaoh V1 pools to generate registry entries
//
// Calls metadata() and getAmountOut() on each pool, then probes storage slots
// to determine the reserve layout. Outputs Go registry entries.
//
// Usage: node evm-quoter/scripts/probe_pharaoh_v1.ts

import { createQuoter } from "../sdk.ts";
import { keccak256, pad, toHex, encodeFunctionData, decodeFunctionResult, parseAbi, hexToBigInt } from "viem";

const STATE_SERVER = "ws://127.0.0.1:7449";

// Missing pools (from registry.txt:1 but not in pharaoh_v1_registry.go)
const MISSING_POOLS = [
  "0xc26847bfa980a72c82e924899a989c47b088d7da",
  "0x60990d5b305b8b2f53cdfdfcb705ba6f08b88b92",
  "0x57167b368afbd16413e0920a80e3af16bf728540",
  "0xc7a712c645c2e9e39fd234bbaf778d09fef47c4e",
  "0x2cc00706cb6b2c927be3704efa5a4639dd214a8d",
];

// metadata() selector: keccak256("metadata()")[:4]
const METADATA_SELECTOR = keccak256(new TextEncoder().encode("metadata()")).slice(0, 10);
// getAmountOut(uint256,address) selector
const GET_AMOUNT_OUT_SELECTOR = keccak256(new TextEncoder().encode("getAmountOut(uint256,address)")).slice(0, 10);
// fee() selector
const FEE_SELECTOR = keccak256(new TextEncoder().encode("fee()")).slice(0, 10);

// Known reserve slot patterns to try
const SLOT_PATTERNS = [
  { name: "packed:11", packed: 11, r0: -1, r1: -1 },
  { name: "separate:16,17", packed: -1, r0: 16, r1: 17 },
  { name: "separate:8,9", packed: -1, r0: 8, r1: 9 },
  { name: "separate:9,10", packed: -1, r0: 9, r1: 10 },
  { name: "separate:7,8", packed: -1, r0: 7, r1: 8 },
];

interface PoolResult {
  pool: string;
  stable: boolean;
  decimals0: bigint;
  decimals1: bigint;
  reserve0: bigint;
  reserve1: bigint;
  token0: string;
  token1: string;
  fee: number;
  subtractOne: boolean;
  r0Slot: number;
  r1Slot: number;
  packedSlot: number;
  error?: string;
}

async function main() {
  const quoter = await createQuoter("native", { stateServerUrl: STATE_SERVER });

  const results: PoolResult[] = [];
  const errors: { pool: string; error: string }[] = [];

  for (const pool of MISSING_POOLS) {
    try {
      const result = await probePool(quoter, pool);
      if (result.error) {
        errors.push({ pool, error: result.error });
      } else {
        results.push(result);
      }
    } catch (e) {
      errors.push({ pool, error: (e as Error).message });
    }
  }

  // Output Go registry entries
  console.log("\n=== Go registry entries ===\n");
  for (const r of results.sort((a, b) => a.pool.localeCompare(b.pool))) {
    const stable = r.stable ? "true" : "false";
    const sub1 = r.subtractOne ? "true" : "false";
    console.log(`\t"${r.pool}": {${stable}, ${r.decimals0}, ${r.decimals1}, ${r.fee}, ${sub1}, ${r.r0Slot}, ${r.r1Slot}, ${r.packedSlot}},`);
  }

  if (errors.length > 0) {
    console.log(`\n=== Errors (${errors.length}) ===\n`);
    for (const e of errors) {
      console.log(`  ${e.pool}: ${e.error}`);
    }
  }

  console.log(`\n${results.length} OK, ${errors.length} errors`);
  quoter.close();
}

async function probePool(quoter: any, pool: string): Promise<PoolResult> {
  // 1. Call metadata() to get decimals, reserves, stable, tokens
  const metaResult = await quoter.ethCall({
    to: pool,
    data: METADATA_SELECTOR,
    from: "0x0000000000000000000000000000000000000001",
  });

  if (metaResult.error || !metaResult.returnData || metaResult.returnData === "0x") {
    return { pool, error: `metadata() failed: ${metaResult.error || "empty"}` } as any;
  }

  // Decode metadata(): returns (uint256 dec0, uint256 dec1, uint256 r0, uint256 r1, bool stable, address token0, address token1)
  const data = metaResult.returnData as string;
  const dec0 = hexToBigInt(("0x" + data.slice(2, 66)) as `0x${string}`);
  const dec1 = hexToBigInt(("0x" + data.slice(66, 130)) as `0x${string}`);
  const r0 = hexToBigInt(("0x" + data.slice(130, 194)) as `0x${string}`);
  const r1 = hexToBigInt(("0x" + data.slice(194, 258)) as `0x${string}`);
  const stable = hexToBigInt(("0x" + data.slice(258, 322)) as `0x${string}`) !== 0n;
  const token0 = "0x" + data.slice(346, 386);
  const token1 = "0x" + data.slice(410, 450);

  if (r0 === 0n || r1 === 0n) {
    return { pool, error: "zero reserves" } as any;
  }

  // 2. Detect fee using getAmountOut with a test amount
  let testAmount = r0 / 1000n; // use 0.1% of reserve as test
  if (testAmount === 0n) {
    testAmount = r0 > 0n ? r0 : 1n; // fallback to full reserve or 1
  }

  const testAmountHex = pad(toHex(testAmount), { size: 32 });
  const token0Padded = pad(token0 as `0x${string}`, { size: 32 });
  const getAmountOutData = GET_AMOUNT_OUT_SELECTOR + testAmountHex.slice(2) + token0Padded.slice(2);

  const amountOutResult = await quoter.ethCall({
    to: pool,
    data: getAmountOutData,
    from: "0x0000000000000000000000000000000000000001",
  });

  let fee = 0;
  let subtractOne = false;

  if (amountOutResult.error || !amountOutResult.returnData) {
    fee = await readFeeFromContract(quoter, pool);
  } else {
    const evmOut = hexToBigInt(amountOutResult.returnData as `0x${string}`);
    // Reverse-engineer fee by trying different bps values
    const { detectedFee, detectedSub1 } = detectFee(testAmount, r0, r1, dec0, dec1, stable, evmOut);
    fee = detectedFee;
    subtractOne = detectedSub1;
    // If detection returned 0 and evmOut differs from zero-fee calc, try fee() fallback
    if (fee === 0 && evmOut !== testAmount * r1 / (r0 + testAmount)) {
      const fallbackFee = await readFeeFromContract(quoter, pool);
      if (fallbackFee > 0) fee = fallbackFee;
    }
  }

  // 3. Detect storage layout by reading slots and matching reserves
  const layout = await detectLayout(quoter, pool, r0, r1);

  return {
    pool,
    stable,
    decimals0: dec0,
    decimals1: dec1,
    reserve0: r0,
    reserve1: r1,
    token0,
    token1,
    fee,
    subtractOne,
    ...layout,
  };
}

function detectFee(
  amountIn: bigint, r0: bigint, r1: bigint,
  dec0: bigint, dec1: bigint, stable: boolean,
  evmOut: bigint,
): { detectedFee: number; detectedSub1: boolean } {
  // Try fee values from 0 to 5000 bps
  for (let feeBps = 0; feeBps <= 5000; feeBps++) {
    const adjusted = amountIn - (amountIn * BigInt(feeBps) / 10000n);
    let out: bigint;
    if (stable) {
      // Use simplified constant-product for fee detection (close enough for bps detection)
      out = adjusted * r1 / (r0 + adjusted);
    } else {
      out = adjusted * r1 / (r0 + adjusted);
    }

    if (out === evmOut) {
      return { detectedFee: feeBps, detectedSub1: false };
    }
    if (out > 0n && out - 1n === evmOut) {
      return { detectedFee: feeBps, detectedSub1: true };
    }
  }

  // Fallback: try reading fee() from contract
  return { detectedFee: 0, detectedSub1: false };
}

async function detectLayout(
  quoter: any, pool: string,
  expectedR0: bigint, expectedR1: bigint,
): Promise<{ r0Slot: number; r1Slot: number; packedSlot: number }> {
  // Try packed slot 11 first
  const slot11Result = await readStorageSlot(quoter, pool, 11);
  if (slot11Result !== 0n) {
    const mask112 = (1n << 112n) - 1n;
    const r0 = slot11Result & mask112;
    const r1 = (slot11Result >> 112n) & mask112;
    if (r0 === expectedR0 && r1 === expectedR1) {
      return { r0Slot: -1, r1Slot: -1, packedSlot: 11 };
    }
  }

  // Try separate slot patterns
  for (const pattern of SLOT_PATTERNS) {
    if (pattern.packed >= 0) continue; // already tried packed
    const s0 = await readStorageSlot(quoter, pool, pattern.r0);
    const s1 = await readStorageSlot(quoter, pool, pattern.r1);
    if (s0 === expectedR0 && s1 === expectedR1) {
      return { r0Slot: pattern.r0, r1Slot: pattern.r1, packedSlot: -1 };
    }
  }

  // Fallback: none matched, try separate:16,17 as default
  console.error(`  WARNING: ${pool} no slot pattern matched r0=${expectedR0} r1=${expectedR1}`);
  return { r0Slot: 16, r1Slot: 17, packedSlot: -1 };
}

async function readFeeFromContract(quoter: any, pool: string): Promise<number> {
  const feeResult = await quoter.ethCall({
    to: pool,
    data: FEE_SELECTOR,
    from: "0x0000000000000000000000000000000000000001",
  });
  if (!feeResult.error && feeResult.returnData && feeResult.returnData !== "0x") {
    const feeVal = hexToBigInt(feeResult.returnData as `0x${string}`);
    // fee() returns per-million on newer Pharaoh contracts (e.g. 500 = 0.05%)
    if (feeVal > 10000n) {
      return Number(feeVal / 100n); // per-million to bps
    }
    return Number(feeVal);
  }
  return 0;
}

async function readStorageSlot(quoter: any, pool: string, slot: number): Promise<bigint> {
  // Use eth_call with a SLOAD trick: call getStorageAt-like function
  // Actually, we can use state server directly. The quoter doesn't expose getStorageAt,
  // but we can use a contract call that reads a specific slot.
  // Simpler: just use eth_call to call a special contract that does SLOAD.
  // OR: use the stateOverrides approach with a known extcodecopy trick.
  //
  // Actually, for simplicity, let's deploy a tiny helper that does SLOAD via
  // state override. We override code at a dummy address to be a SLOAD returner.

  // SLOAD helper bytecode:
  // CALLDATALOAD(0) -> slot | SLOAD -> value | MSTORE(0, value) | RETURN(0, 32)
  // 600035545f5260205ff3
  const HELPER = "0x0000000000000000000000000000000000000099";
  const slotHex = pad(toHex(slot), { size: 32 });

  const result = await quoter.ethCall({
    to: HELPER,
    data: slotHex,
    from: pool, // set msg.sender to pool so SLOAD reads pool's storage? No, SLOAD reads HELPER's storage
  });

  // Actually this won't work - SLOAD reads the called contract's storage, not the pool's.
  // We need a DELEGATECALL approach or just use state server's getStorageAt.

  // Let's use a simpler approach: call a view function on the pool that returns storage.
  // Or better: use STATICCALL with code override at the pool address? No, that would break metadata.

  // The cleanest approach: use the state server's getStorageAt directly via WebSocket
  // But our quoter API doesn't expose that.

  // Alternative: read reserves from known view functions instead of raw storage.
  // The pool has reserve0() and reserve1() view functions in some implementations.

  // For now, let's use a different approach: override code at HELPER to do:
  // PUSH20 pool | PUSH1 slot | PUSH0 | MSTORE | PUSH1 0x20 | PUSH0 | PUSH20 pool | GAS | STATICCALL | ...
  // This is getting complicated. Let me use a codecopy approach.

  // Actually the simplest: use a helper contract that does:
  //   target = address from calldata[0:20]
  //   slot = calldata[32:64]
  //   EXTCODECOPY... no, we need SLOAD on the target.

  // The ONLY way to read another contract's storage in EVM is via a view function call.
  // So let's override the pool's code to be a simple SLOAD-and-return, read the slot,
  // then the pool code gets restored on the next call (no state override).

  // OK let me just do this: override the pool code to return SLOAD(calldata[0:32])
  // Bytecode: 5f 35 54 5f 52 60 20 5f f3
  // PUSH0 | CALLDATALOAD | SLOAD | PUSH0 | MSTORE | PUSH1 0x20 | PUSH0 | RETURN
  const SLOAD_CODE = "0x5f355460005260206000f3";

  const sloadResult = await quoter.ethCall({
    to: pool,
    data: slotHex,
    from: "0x0000000000000000000000000000000000000001",
    stateOverrides: {
      [pool]: { code: SLOAD_CODE },
    },
  });

  if (sloadResult.error || !sloadResult.returnData) {
    return 0n;
  }

  return hexToBigInt(sloadResult.returnData as `0x${string}`);
}

main().catch(e => { console.error(e); process.exit(1); });
