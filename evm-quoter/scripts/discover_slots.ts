// discover_slots.ts — Brute-force ERC20 balance slot discovery
// For each token, tries slots 0..MAX_SLOT, sets keccak256(router, slot) to a
// known value via state override, calls balanceOf(router). Match = found slot.
//
// Verification: each discovered slot is verified with a second probe value.
// Only verified slots are appended to token_overrides.json.
//
// Usage: node discover_slots.ts [limit] [--write]
//   limit: number of tokens to probe (default 50)
//   --write: actually write to token_overrides.json (dry run without)

import { readFileSync, writeFileSync } from "node:fs";
import { keccak256, pad, toHex } from "viem";
import { createQuoter, ROUTER } from "../sdk.ts";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const REPO = join(__dirname, "../..");

const STATE_SERVER = "ws://127.0.0.1:7449/live";
const OVERRIDES_PATH = join(REPO, "router/data/token_overrides.json");
const POOLS_PATH = join(REPO, "pool-collector/data/pools.txt");

const MAX_SLOT = 100;
// Use very large values — reflect tokens need _rOwned to be comparable to _rTotal
const PROBE_A_HEX = pad(toHex((2n ** 255n) - 1n), { size: 32 });
const PROBE_B_HEX = pad(toHex((2n ** 254n) - 1n), { size: 32 });

// balanceOf(address) selector = 0x70a08231
const balanceOfData = "0x70a08231" + pad(ROUTER, { size: 32 }).slice(2);

const doWrite = process.argv.includes("--write");
const limit = parseInt(process.argv.find(a => /^\d+$/.test(a)) || "50", 10);

// ── Load existing overrides ──────────────────────────────────────────

const existing = JSON.parse(readFileSync(OVERRIDES_PATH, "utf-8"));
const knownTokens = new Set(existing.map(o => o.address.toLowerCase()));

// ── Load pools up to limit, find missing tokens within those pools ───

const allLines = readFileSync(POOLS_PATH, "utf-8").split("\n").filter(l => l.includes(":"));
const poolLines = allLines.slice(0, limit);
const tokenPools = new Map(); // token → count of pools (within our pool set)

for (const line of poolLines) {
  const p = line.split(":");
  if (p.length < 6) continue;
  const t0 = p[4].toLowerCase(), t1 = p[5].split(":")[0].toLowerCase();
  for (const t of [t0, t1]) {
    if (!knownTokens.has(t) && t !== "0x0000000000000000000000000000000000000000") {
      tokenPools.set(t, (tokenPools.get(t) || 0) + 1);
    }
  }
}

const targets = [...tokenPools.entries()]
  .sort((a, b) => b[1] - a[1])
  .map(([addr, count]) => ({ addr, count }));

console.log(`${limit} pools → ${targets.length} missing tokens, probing slots 0..${MAX_SLOT}`);
console.log(`Mode: ${doWrite ? "WRITE" : "DRY RUN (pass --write to save)"}\n`);

// ── Helpers ──────────────────────────────────────────────────────────

function storageKey(holder, slot) {
  return keccak256(pad(holder, { size: 32 }) + pad(toHex(slot), { size: 32 }).slice(2));
}

// Minimum balance we need — 1e18 is plenty for any token
const MIN_BALANCE = 10n ** 18n;

async function probeBalance(backend, token, slot, probeHex) {
  const key = storageKey(ROUTER, slot);
  const result = await backend.ethCall({
    to: token,
    data: balanceOfData,
    from: "0x0000000000000000000000000000000000000001",
    stateOverrides: { [token]: { stateDiff: { [key]: probeHex } } },
  });
  if (!result.error && result.returnData) {
    try { return BigInt(result.returnData); } catch {}
  }
  return 0n;
}

// ── Execute ──────────────────────────────────────────────────────────

const native = await createQuoter("native", { stateServerUrl: STATE_SERVER });

const verified = [];
const notFound = [];
let probeCount = 0;

for (let ti = 0; ti < targets.length; ti++) {
  const token = targets[ti].addr;
  let foundSlot = null;

  // Pass 1: find slot where setting it gives balance >= MIN_BALANCE
  for (let slot = 0; slot <= MAX_SLOT; slot++) {
    probeCount++;
    const bal = await probeBalance(native, token, slot, PROBE_A_HEX);
    if (bal >= MIN_BALANCE) {
      foundSlot = slot;
      break;
    }
  }

  if (foundSlot === null) {
    console.log(`  ${token}  ${String(targets[ti].count).padStart(4)} pools  NOT FOUND`);
    notFound.push(targets[ti]);
    continue;
  }

  // Pass 2: verify with different probe value — balance should still be >= MIN_BALANCE
  probeCount++;
  const bal2 = await probeBalance(native, token, foundSlot, PROBE_B_HEX);

  if (bal2 >= MIN_BALANCE) {
    console.log(`  ${token}  ${String(targets[ti].count).padStart(4)} pools  VERIFIED slot=${foundSlot}`);
    verified.push({ address: token, slot: foundSlot, pools: targets[ti].count });
  } else {
    console.log(`  ${token}  ${String(targets[ti].count).padStart(4)} pools  FOUND slot=${foundSlot} BUT VERIFY FAILED`);
    notFound.push(targets[ti]);
  }
}

console.log(`\nProbed ${probeCount} calls`);
console.log(`Verified: ${verified.length}/${targets.length}, not found: ${notFound.length}\n`);

// ── Write results ────────────────────────────────────────────────────

if (verified.length > 0) {
  let unblocked = 0;
  console.log("=== Verified slots ===\n");
  for (const f of verified) {
    unblocked += f.pools;
    console.log(`  ${f.address}  slot=${f.slot}  (${f.pools} pools, cumulative: ${unblocked})`);
  }
  console.log(`\nTotal pools unblocked: ${unblocked}`);

  if (doWrite) {
    // Re-read to avoid race conditions
    const current = JSON.parse(readFileSync(OVERRIDES_PATH, "utf-8"));
    const currentSet = new Set(current.map(o => o.address.toLowerCase()));
    let added = 0;

    for (const v of verified) {
      if (!currentSet.has(v.address.toLowerCase())) {
        current.push({ address: v.address, slot: v.slot });
        currentSet.add(v.address.toLowerCase());
        added++;
      }
    }

    writeFileSync(OVERRIDES_PATH, JSON.stringify(current, null, 2) + "\n");
    console.log(`\nWrote ${added} new entries to ${OVERRIDES_PATH}`);
    console.log(`Total entries: ${current.length}`);
  }
}

if (notFound.length > 0) {
  console.log(`\n=== Not found (${notFound.length}) ===\n`);
  for (const nf of notFound) {
    console.log(`  ${nf.addr}  ${nf.count} pools`);
  }
}

native.close();
