import { keccak256, pad, toHex, maxUint256, type Hex, type PublicClient } from "viem";
import * as fs from "fs";
import path from "path";

interface TokenOverrideEntry {
  address: string;
  slot: number;
  allowance_slot?: number;
  erc7201_base?: string;
  erc7201_allowance?: string;
  shift?: number;
  /** For reflection tokens: slot of the _rOwned mapping */
  rOwnedSlot?: number;
  /** For reflection tokens: raw storage slot of _rTotal */
  rTotalSlot?: number;
  /** For reflection tokens: raw storage slot of _tTotal */
  tTotalSlot?: number;
  /** Contract whose code should be overridden with a no-op for swaps to work
   *  (e.g. broken staking hooks). Address of the external hook contract. */
  hookContract?: string;
  /** Raw storage slots on the token contract to zero out (e.g. disable maxWallet checks) */
  disableSlots?: number[];
}

let _overrides: Map<string, TokenOverrideEntry> | null = null;

function loadOverrides(): Map<string, TokenOverrideEntry> {
  if (_overrides) return _overrides;
  const jsonPath = path.join(import.meta.dirname, "data/token_overrides.json");
  const entries: TokenOverrideEntry[] = JSON.parse(fs.readFileSync(jsonPath, "utf-8"));
  _overrides = new Map();
  for (const e of entries) {
    _overrides.set(e.address.toLowerCase(), e);
  }
  return _overrides;
}

/** keccak256(abi.encode(address, uint256)) — standard mapping slot */
function keccak256AddressUint(addr: string, slot: number): Hex {
  const addrPadded = pad(addr as Hex, { size: 32 });
  const slotPadded = pad(toHex(slot), { size: 32 });
  return keccak256(`${addrPadded}${slotPadded.slice(2)}` as Hex);
}

/** keccak256(abi.encode(address, bytes32)) — ERC-7201 or nested mapping */
function keccak256AddressHash(addr: string, hash: Hex): Hex {
  const addrPadded = pad(addr as Hex, { size: 32 });
  const hashPadded = pad(hash, { size: 32 });
  return keccak256(`${addrPadded}${hashPadded.slice(2)}` as Hex);
}

function computeBalanceSlot(holder: string, entry: TokenOverrideEntry): Hex {
  if (entry.erc7201_base) {
    return keccak256AddressHash(holder, entry.erc7201_base as Hex);
  }
  return keccak256AddressUint(holder, entry.slot);
}

function computeAllowanceSlot(owner: string, spender: string, entry: TokenOverrideEntry): Hex {
  let innerHash: Hex;
  if (entry.erc7201_base) {
    const base = (entry.erc7201_allowance || entry.erc7201_base) as Hex;
    innerHash = keccak256AddressHash(owner, base);
  } else {
    const allowanceSlot = entry.allowance_slot ?? entry.slot + 1;
    innerHash = keccak256AddressUint(owner, allowanceSlot);
  }
  return keccak256AddressHash(spender, innerHash);
}

function encodeAmount(amount: bigint, shift?: number): Hex {
  const value = shift ? amount << BigInt(shift) : amount;
  return pad(toHex(value), { size: 32 });
}

/**
 * Get a viem-compatible state override that gives `holder` a balance of `amount`
 * for the specified token. Uses token_overrides.json to compute the storage slot.
 *
 * Returns an object ready to spread into eth_call stateOverride.
 */
export function getBalanceOverride(
  token: string,
  amount: bigint,
  holder?: string,
): Record<string, { stateDiff: Record<string, Hex> }> {
  const addr = token.toLowerCase();
  const overrides = loadOverrides();
  const entry = overrides.get(addr);
  if (!entry) return {};

  const holderAddr = (holder || token).toLowerCase();
  const slot = computeBalanceSlot(holderAddr, entry);
  const value = encodeAmount(amount, entry.shift);

  const stateDiff: Record<string, Hex> = { [slot]: value };
  if (entry.disableSlots) {
    for (const ds of entry.disableSlots) {
      stateDiff[pad(toHex(ds), { size: 32 })] = pad("0x0", { size: 32 });
    }
  }

  return { [addr]: { stateDiff } };
}

/**
 * Get a viem-compatible state override that sets allowance[owner][spender] = amount
 * for the specified token.
 */
export function getAllowanceOverride(
  token: string,
  owner: string,
  spender: string,
  amount: bigint = maxUint256,
): Record<string, { stateDiff: Record<string, Hex> }> {
  const addr = token.toLowerCase();
  const overrides = loadOverrides();
  const entry = overrides.get(addr);
  if (!entry) return {};

  const slot = computeAllowanceSlot(owner.toLowerCase(), spender.toLowerCase(), entry);
  const value = pad(toHex(amount), { size: 32 });

  return { [addr]: { stateDiff: { [slot]: value } } };
}

/**
 * Check if a token is a reflection token (needs async balance override).
 */
export function isReflectionToken(token: string): boolean {
  const entry = loadOverrides().get(token.toLowerCase());
  return entry?.rOwnedSlot !== undefined;
}

/**
 * Async balance override for reflection tokens. Reads _rTotal and _tTotal from
 * chain to compute the correct _rOwned value: rOwned = amount * (_rTotal / _tTotal).
 * Falls back to synchronous getBalanceOverride for non-reflection tokens.
 */
export async function getBalanceOverrideAsync(
  client: PublicClient,
  token: string,
  amount: bigint,
  holder: string,
  blockNumber?: bigint,
): Promise<Record<string, { stateDiff: Record<string, Hex> }>> {
  const addr = token.toLowerCase();
  const overrides = loadOverrides();
  const entry = overrides.get(addr);
  if (!entry) return {};

  if (entry.rOwnedSlot === undefined || entry.rTotalSlot === undefined || entry.tTotalSlot === undefined) {
    return getBalanceOverride(token, amount, holder);
  }

  // Read _rTotal and _tTotal from chain
  const [rTotalHex, tTotalHex] = await Promise.all([
    client.getStorageAt({
      address: addr as `0x${string}`,
      slot: pad(toHex(entry.rTotalSlot), { size: 32 }) as `0x${string}`,
      blockNumber,
    }),
    client.getStorageAt({
      address: addr as `0x${string}`,
      slot: pad(toHex(entry.tTotalSlot), { size: 32 }) as `0x${string}`,
      blockNumber,
    }),
  ]);

  const rTotal = BigInt(rTotalHex!);
  const tTotal = BigInt(tTotalHex!);
  if (tTotal === 0n) return getBalanceOverride(token, amount, holder);

  const rate = rTotal / tTotal;
  const rOwned = amount * rate;

  const holderAddr = holder.toLowerCase();
  const slot = keccak256AddressUint(holderAddr, entry.rOwnedSlot);

  return { [addr]: { stateDiff: { [slot]: pad(toHex(rOwned), { size: 32 }) } } };
}

/**
 * Get hook contract code overrides needed for a token to transfer correctly.
 * Some tokens have _afterTokenTransfer hooks that call external contracts
 * (e.g. staking contracts). Override those with a no-op to prevent reverts.
 *
 * Returns { [hookAddr]: { code: "0x..." } } or empty object.
 */
export function getHookOverrides(token: string): Record<string, { code: Hex }> {
  const addr = token.toLowerCase();
  const overrides = loadOverrides();
  const entry = overrides.get(addr);
  if (!entry?.hookContract) return {};

  // Dummy runtime: PUSH1 1, PUSH1 0, MSTORE, PUSH1 0x20, PUSH1 0, RETURN
  // Returns 32 bytes with value 1 (true) for any call
  const dummyCode = "0x600160005260206000f3" as Hex;
  return { [entry.hookContract.toLowerCase()]: { code: dummyCode } };
}
