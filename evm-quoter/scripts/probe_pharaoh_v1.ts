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
  "0x00f12d3aaea17c3f6e7fcf7c7784a31f026e7dbc",
  "0x037822d5191d48059bfb2c478e2340566b39aaa6",
  "0x13b02ea458bab7888ca2196225724ca510904d69",
  "0x13e4a7f12c72f0a791b4e6831b2e3a6aa2680b9a",
  "0x1b0ee257a31efb9b39204b22926b21d2627dada5",
  "0x25ec6dd6a4d689bf66b846dcd2a52e94597953f5",
  "0x27f102a90f59d9b6537a67c9b00bbea6a4840e56",
  "0x291fc29a112ead76bc96781fbb098cafe4126772",
  "0x302ad40f962c6bf4c7a487689b48c15760277e63",
  "0x316703e4e98238da92943a539ef95dc5f47a4455",
  "0x3612ca1ce7f4c62dc2b92298ac40ab39fa7081c6",
  "0x3d01c683b3db0f3b11c63ff3ef002bf41a26c8a6",
  "0x3d080809d4824a64d7d5234a316f6ab533628863",
  "0x3ebc1d87e55780e198266744881f6884a2a67224",
  "0x40557c1788ed15d8fdcebd61275b877079543058",
  "0x55ee0524b6b7345fe549eece23f7ff9e7fb8a01b",
  "0x580798fa09622151c50b1044532b416a2426c579",
  "0x6770e1f41b56c836815233e047b07491bd07217f",
  "0x6a493728697f6c139423775354b2ee48648fb7af",
  "0x811c464e3718492f493cca690b56e6d409bfb3a9",
  "0x81c7d43e71f17972294e95a31ad3372a718d390a",
  "0x830c52655f638f78a9fbbf1ac29f854176adf239",
  "0x8840fe37562d00a4e89ef7e14a02dacff670671d",
  "0x8a39acacb5da8fc4aefdcaeeca9adf09758931da",
  "0x8bf341715190976e703ab3babca3dbaa9433e787",
  "0x8ecb3fa33b9c1247c9b233c6e00d129d9ebb0acf",
  "0x94fd3f9200414d2b0af6288e08dc2f27b2f75745",
  "0x955514594f1b43514156c1c9e7a89cbd4815c172",
  "0x9bc83c2a02ddb9dc0b9d79f167182e546ed99b8b",
  "0x9e2015cb486d148beb09340776ae5403e29afc41",
  "0x9e8db550be1846b891b5c55fbd29755933aa2aab",
  "0xa3ae4168b59acc579890f572951008e95b40900d",
  "0xa4954ed04109df01de4db32f6694984f5e6696b0",
  "0xadcce9a15613649c6e83e36033de5893fda9b2af",
  "0xbf12f8e274322bdd7cec20e031cd09b428abdc33",
  "0xc7581d3f3f6aaedd801ba45939ffce91f6a96c68",
  "0xd7fcf48ef8d1440f12b46d2782e672c367126284",
  "0xeea7464b19a03fdca6c8decfed0fcc148ceb0c8b",
  "0xf4067749a859209a9626d66b595bcefdc978e7e5",
  "0xf99a6ae45f3f1af9ca29d9317c84995ce6cb355c",
  "0xfb7119aab191162d55962b53b00b2fc38acf053b",
  "0xff5ef6e04478f656c24020bec678afb3d9f2a380",
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
