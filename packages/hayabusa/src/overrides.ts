import { readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { keccak256, pad, toHex, maxUint256, type Hex } from "viem";

const __dirname = dirname(fileURLToPath(import.meta.url));
const DATA_DIR = join(__dirname, "..", "data");

interface TokenOverrideEntry {
  address: string;
  slot: number;
  allowance_slot?: number;
  shift?: number;
  hookContract?: string;
  disableSlots?: number[];
}

let _overrides: Map<string, TokenOverrideEntry> | null = null;

function getOverrides(): Map<string, TokenOverrideEntry> {
  if (_overrides) return _overrides;
  const jsonPath = join(DATA_DIR, "token_overrides.json");
  const entries: TokenOverrideEntry[] = JSON.parse(readFileSync(jsonPath, "utf-8"));
  _overrides = new Map(entries.map(e => [e.address.toLowerCase(), e]));
  return _overrides;
}

/** Compute the storage slot for an ERC20 balance override */
export function getBalanceOverride(
  token: string,
  amount: bigint,
  holder?: string,
): { slot: Hex; value: Hex } | null {
  const entry = getOverrides().get(token.toLowerCase());
  if (!entry) return null;

  const holderAddr = holder || "0x000000000000000000000000000000000000dEaD";
  const slot = keccak256(
    pad(holderAddr as Hex, { size: 32 }) +
    pad(toHex(entry.slot), { size: 32 }).slice(2) as Hex
  );

  let value: Hex;
  if (entry.shift) {
    // Shifted storage (e.g. packed with other fields)
    const shifted = amount << BigInt(entry.shift);
    value = pad(toHex(shifted), { size: 32 });
  } else {
    value = pad(toHex(amount), { size: 32 });
  }

  return { slot, value };
}

/** Build state overrides for the router to have token balances */
export function buildStateOverrides(opts: {
  routerAddress: string;
  tokenAmounts: Map<string, bigint>;
}): Record<string, any> {
  const { routerAddress, tokenAmounts } = opts;
  const overrides: Record<string, any> = {};

  // Give router AVAX balance
  overrides[routerAddress] = {
    balance: toHex(maxUint256),
  };

  // Token balance overrides
  for (const [token, amount] of tokenAmounts) {
    const result = getBalanceOverride(token, amount, routerAddress);
    if (!result) continue;

    if (!overrides[token]) {
      overrides[token] = { stateDiff: {} };
    }
    if (!overrides[token].stateDiff) {
      overrides[token].stateDiff = {};
    }
    overrides[token].stateDiff[result.slot] = result.value;
  }

  // Hook overrides (disable broken contracts)
  const ovr = getOverrides();
  for (const [token] of tokenAmounts) {
    const entry = ovr.get(token.toLowerCase());
    if (!entry) continue;

    if (entry.hookContract) {
      const hook = entry.hookContract.toLowerCase();
      if (!overrides[hook]) overrides[hook] = {};
      // Replace hook code with a no-op (STOP)
      overrides[hook].code = "0x00";
    }

    if (entry.disableSlots) {
      if (!overrides[token]) overrides[token] = { stateDiff: {} };
      if (!overrides[token].stateDiff) overrides[token].stateDiff = {};
      for (const slot of entry.disableSlots) {
        overrides[token].stateDiff[pad(toHex(slot), { size: 32 })] = pad("0x0", { size: 32 });
      }
    }
  }

  return overrides;
}
