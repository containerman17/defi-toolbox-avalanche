import { createPublicClient, http, keccak256, encodePacked, encodeAbiParameters, parseAbiParameters, pad, toHex, fromHex, type Address, type Hex } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({
  chain: avalanche,
  transport: http("http://localhost:9650/ext/bc/C/rpc"),
});

const TOKENS: Address[] = [
  "0x00697f5f6dc2ca0a17e6c89bccd1173a61ea24a6",
  "0x0256b279d973c8d687264ac3eb36be09232d4474",
  "0x03e8d118a1864c7dc53bf91e007ab7d91f5a06fa",
  "0x0a10d108e2d81ccc793e37b56206c84bf96ddc57",
  "0x13af0fe9eb35e91758b467f95cbc78e16fdd8b6b",
  "0x153374c6d6786b6ca2c4bc96f9c3a471428f2bc7",
  "0x2d0afed89a6d6a100273db377dba7a32c739e314",
  "0x4586af10ecceed4e383e3f2ec93b6c61e26500b5",
  "0x566445f0d154d09573ec0ae4e373c8340d41548e",
  "0x5684a087c739a2e845f4aaaabf4fbd261edc2be8",
  "0x5ddc8d968a94cf95cfeb7379f8372d858b9c797d",
  "0x62edc0692bd897d2295872a9ffcac5425011c661",
  "0x6d923f688c7ff287dc3a5943caeefc994f97b290",
  "0x6edac263561da41ade155a992759260fafb87b43",
  "0x6f43ff77a9c0cf552b5b653268fbfe26a052429b",
  "0x700103766c23fb8da956caf94c756b90106eeb41",
  "0x91870b9c25c06e10bcb88bdd0f7b43a13c2d7c41",
  "0x96e1056a8814de39c8c3cd0176042d6cecd807d7",
  "0x99f2bdf00acd067c65a79a0b6a3914c555196ea4",
  "0x9e15f045e44ea5a80e7fbc193a35287712cc5569",
  "0x9e6832d13b29d0b1c1c3465242681039b31c7a05",
  "0xabe7a9dfda35230ff60d1590a929ae0644c47dc1",
  "0xac6e53f1e1ebafda8553c0add8c5b32bcb5890c4",
  "0xb0aa388a35742f2d54a049803bff49a70eb99659",
  "0xc0c5aa69dbe4d6dddfbc89c0957686ec60f24389",
  "0xc5718c4c91e9d051b082e02b1138ab5b6a01dcd1",
  "0xca2e0f72653337d05b1abcebea5718a4a3e57a0b",
  "0xd05ee0206142342fd3718cd62a95e124c9f0cfcd",
  "0xd7ef0b8763a9053c051f1d86b317922243a03099",
  "0xdf50ad73b92c758bbf94869b4b7b9128bbe4a475",
  "0xebb5d4959b2fba6318fbda7d03cd44ae771fc999",
  "0xf3dd4e0a1db7c5dcbf3b225698cb6a916aeb24d9",
  "0xfc6da929c031162841370af240dec19099861d3b",
  "0xfe6b19286885a4f7f55adad09c3cd1f906d2478f",
];

// Well-known addresses likely to hold tokens
const KNOWN_HOLDERS: Address[] = [
  "0x9702230A8Ea53601f5cD2dc00fDBc13d4dF4A8c7", // USDt
  "0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E", // USDC
  "0x60aE616a2155Ee3d9A68541Ba4544862310933d4", // TraderJoe Router
  "0xdef171fe48cf0115b1d80b88dc8eab59176fee57", // Paraswap
  "0x1111111254EEB25477B68fb85Ed929f73A960582", // 1inch
  "0x0000000000000000000000000000000000000001",
  "0x0000000000000000000000000000000000000000",
  "0xdeaddeaddeaddeaddeaddeaddeaddeaddeaddead",
];

const ERC7201_SLOT = "0x52c63247e1f47db19d5ce0460030c497f067ca4cebf71ba98eeadabe20bace00" as Hex;

const BALANCEOF_SIG = "0x70a08231" as Hex;

// Standard slots to try first
const STANDARD_SLOTS = [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 51, 52, 101, 151, 201, 208, 516];

const BLOCK = "0x4ce4002" as Hex;

function computeStorageSlot(holder: Address, slot: bigint): Hex {
  return keccak256(encodeAbiParameters(
    parseAbiParameters("address, uint256"),
    [holder, slot]
  ));
}

function computeERC7201Slot(holder: Address): Hex {
  return keccak256(encodeAbiParameters(
    parseAbiParameters("address, bytes32"),
    [holder, ERC7201_SLOT]
  ));
}

async function getBalance(token: Address, holder: Address): Promise<bigint> {
  try {
    const data = await client.call({
      to: token,
      data: (BALANCEOF_SIG + holder.slice(2).padStart(64, "0")) as Hex,
      blockNumber: fromHex(BLOCK, "bigint"),
    });
    if (data.data && data.data !== "0x" && data.data.length >= 66) {
      return fromHex(data.data as Hex, "bigint");
    }
    return 0n;
  } catch {
    return 0n;
  }
}

async function getStorageAt(token: Address, storageSlot: Hex): Promise<Hex> {
  try {
    const result = await client.getStorageAt({
      address: token,
      slot: storageSlot,
      blockNumber: fromHex(BLOCK, "bigint"),
    });
    return result || "0x0";
  } catch {
    return "0x0";
  }
}

async function findHolder(token: Address): Promise<{ holder: Address; balance: bigint } | null> {
  // Try known holders first
  for (const holder of KNOWN_HOLDERS) {
    const bal = await getBalance(token, holder);
    if (bal > 0n) return { holder, balance: bal };
  }

  // Try finding via Transfer events - get recent transfers
  try {
    const TRANSFER_TOPIC = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef";
    const currentBlock = fromHex(BLOCK, "bigint");
    const fromBlock = currentBlock - 100000n;

    const logs = await client.request({
      method: "eth_getLogs" as any,
      params: [{
        address: token,
        topics: [TRANSFER_TOPIC],
        fromBlock: toHex(fromBlock),
        toBlock: BLOCK,
      }] as any,
    }) as any[];

    if (logs && logs.length > 0) {
      // Try recipients (topic[2]) from last few logs
      const seen = new Set<string>();
      for (let i = logs.length - 1; i >= Math.max(0, logs.length - 20); i--) {
        const log = logs[i];
        if (log.topics && log.topics[2]) {
          const recipient = ("0x" + log.topics[2].slice(26)) as Address;
          if (seen.has(recipient.toLowerCase())) continue;
          seen.add(recipient.toLowerCase());
          const bal = await getBalance(token, recipient);
          if (bal > 0n) return { holder: recipient, balance: bal };
        }
      }
      // Also try senders
      for (let i = logs.length - 1; i >= Math.max(0, logs.length - 10); i--) {
        const log = logs[i];
        if (log.topics && log.topics[1]) {
          const sender = ("0x" + log.topics[1].slice(26)) as Address;
          if (seen.has(sender.toLowerCase())) continue;
          seen.add(sender.toLowerCase());
          const bal = await getBalance(token, sender);
          if (bal > 0n) return { holder: recipient, balance: bal };
        }
      }
    }
  } catch (e) {
    // ignore
  }

  // Try with wider block range
  try {
    const TRANSFER_TOPIC = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef";
    const currentBlock = fromHex(BLOCK, "bigint");
    const fromBlock = currentBlock - 1000000n;

    const logs = await client.request({
      method: "eth_getLogs" as any,
      params: [{
        address: token,
        topics: [TRANSFER_TOPIC],
        fromBlock: toHex(fromBlock),
        toBlock: BLOCK,
      }] as any,
    }) as any[];

    if (logs && logs.length > 0) {
      const seen = new Set<string>();
      for (let i = logs.length - 1; i >= Math.max(0, logs.length - 30); i--) {
        const log = logs[i];
        if (log.topics && log.topics[2]) {
          const recipient = ("0x" + log.topics[2].slice(26)) as Address;
          if (seen.has(recipient.toLowerCase())) continue;
          seen.add(recipient.toLowerCase());
          const bal = await getBalance(token, recipient);
          if (bal > 0n) return { holder: recipient, balance: bal };
        }
      }
    }
  } catch (e) {
    // ignore
  }

  return null;
}

function matchesBalance(storageValue: Hex, balance: bigint): { match: boolean; shift?: number } {
  if (storageValue === "0x0" || storageValue === "0x" || storageValue === "0x0000000000000000000000000000000000000000000000000000000000000000") {
    return { match: false };
  }

  const storageNum = fromHex(storageValue as Hex, "bigint");

  // Direct match
  if (storageNum === balance) return { match: true };

  // Check if balance is packed at some byte offset (shift)
  const storageHex = storageValue.slice(2).padStart(64, "0");
  const balanceHex = balance.toString(16);

  // Try different bit shifts (for packed storage)
  for (let shift = 1; shift <= 20; shift++) {
    const shifted = storageNum >> BigInt(shift * 8);
    const mask = (1n << 128n) - 1n; // 128-bit mask
    if ((shifted & mask) === balance && balance > 0n) {
      return { match: true, shift };
    }
  }

  return { match: false };
}

async function findSlot(token: Address, holder: Address, balance: bigint): Promise<{ slot: number | string; shift?: number; erc7201?: boolean } | null> {
  // Try standard slots
  for (const slot of STANDARD_SLOTS) {
    const storageKey = computeStorageSlot(holder, BigInt(slot));
    const value = await getStorageAt(token, storageKey);
    const result = matchesBalance(value, balance);
    if (result.match) return { slot, shift: result.shift };
  }

  // Try ERC-7201
  {
    const storageKey = computeERC7201Slot(holder);
    const value = await getStorageAt(token, storageKey);
    const result = matchesBalance(value, balance);
    if (result.match) return { slot: 0, erc7201: true, shift: result.shift };
  }

  // Extended range
  for (let slot = 11; slot <= 50; slot++) {
    const storageKey = computeStorageSlot(holder, BigInt(slot));
    const value = await getStorageAt(token, storageKey);
    const result = matchesBalance(value, balance);
    if (result.match) return { slot, shift: result.shift };
  }

  for (let slot = 53; slot <= 100; slot++) {
    const storageKey = computeStorageSlot(holder, BigInt(slot));
    const value = await getStorageAt(token, storageKey);
    const result = matchesBalance(value, balance);
    if (result.match) return { slot, shift: result.shift };
  }

  for (let slot = 102; slot <= 150; slot++) {
    const storageKey = computeStorageSlot(holder, BigInt(slot));
    const value = await getStorageAt(token, storageKey);
    const result = matchesBalance(value, balance);
    if (result.match) return { slot, shift: result.shift };
  }

  for (let slot = 152; slot <= 200; slot++) {
    const storageKey = computeStorageSlot(holder, BigInt(slot));
    const value = await getStorageAt(token, storageKey);
    const result = matchesBalance(value, balance);
    if (result.match) return { slot, shift: result.shift };
  }

  for (let slot = 202; slot <= 207; slot++) {
    const storageKey = computeStorageSlot(holder, BigInt(slot));
    const value = await getStorageAt(token, storageKey);
    const result = matchesBalance(value, balance);
    if (result.match) return { slot, shift: result.shift };
  }

  for (let slot = 209; slot <= 515; slot++) {
    const storageKey = computeStorageSlot(holder, BigInt(slot));
    const value = await getStorageAt(token, storageKey);
    const result = matchesBalance(value, balance);
    if (result.match) return { slot, shift: result.shift };
  }

  for (let slot = 517; slot <= 1000; slot++) {
    const storageKey = computeStorageSlot(holder, BigInt(slot));
    const value = await getStorageAt(token, storageKey);
    const result = matchesBalance(value, balance);
    if (result.match) return { slot, shift: result.shift };
  }

  return null;
}

async function processToken(token: Address): Promise<string> {
  const holderInfo = await findHolder(token);
  if (!holderInfo) {
    return `${token} SLOT_NOT_FOUND (no holder found)`;
  }

  const { holder, balance } = holderInfo;
  const result = await findSlot(token, holder, balance);

  if (!result) {
    return `${token} SLOT_NOT_FOUND (holder=${holder}, balance=${balance})`;
  }

  let line = `${token} ${result.slot}`;
  if (result.erc7201) line += " erc7201";
  if (result.shift) line += ` shift:${result.shift}`;
  return line;
}

async function main() {
  console.log(`Block: ${BLOCK}`);
  console.log(`Processing ${TOKENS.length} tokens...\n`);

  // Process tokens in batches of 5
  const BATCH_SIZE = 5;
  const results: string[] = [];

  for (let i = 0; i < TOKENS.length; i += BATCH_SIZE) {
    const batch = TOKENS.slice(i, i + BATCH_SIZE);
    const batchResults = await Promise.all(batch.map(t => processToken(t)));
    results.push(...batchResults);
    for (const r of batchResults) console.log(r);
  }

  console.log("\n--- Summary ---");
  const found = results.filter(r => !r.includes("SLOT_NOT_FOUND"));
  const notFound = results.filter(r => r.includes("SLOT_NOT_FOUND"));
  console.log(`Found: ${found.length}/${TOKENS.length}`);
  if (notFound.length > 0) {
    console.log(`Not found: ${notFound.length}`);
    for (const r of notFound) console.log(`  ${r}`);
  }
}

main().catch(console.error);
