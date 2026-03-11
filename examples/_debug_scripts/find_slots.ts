// Find ERC20 balance storage slots by probing keccak256(abi.encode(holder, slot))
// Uses raw JSON-RPC calls and js-sha3 for keccak256

import pkg from 'js-sha3';
const { keccak256 } = pkg;

const RPC = 'http://localhost:9650/ext/bc/C/rpc';

// Token address -> pool address (holder with known balance)
const TOKENS: { address: string; name: string; holder: string }[] = [
  { address: '0x68dd2c79939cef2bf8268118825568f361ea2fc9', name: 'CAPY',   holder: '0x3f97b732953511f313f0739514bbac47ae77c9dd' },
  { address: '0x9913ba363073ca3e9ea0cd296e36b75af9e40bef', name: 'TRESR',  holder: '0x71ddeb079b4deded2eb6651420c816620a1d3cfd' },
  { address: '0xf99516bc189af00ff8effd5a1f2295b67d70a90e', name: 'ART',    holder: '0xb0009fa8971fa293c5997f60cdc52d8baaad5525' },
  { address: '0x80f0c1c49891dcfdd40b6e0f960f84e6042bcb6f', name: 'aDXN',   holder: '0x203be576c8b4bf12d41217e29007a0da666ae9fd' },
  { address: '0x694200a68b18232916353250955be220e88c5cbb', name: 'KOVIN',  holder: '0x96bbdb6811d47b1199d444d507de623a905d63f3' },
  { address: '0xef282b38d1ceab52134ca2cc653a569435744687', name: 'WRP',    holder: '0xa4c81fb39ebf487cbd97e8b1c066c9fc04488c00' },
  { address: '0xcac4904e1db1589aa17a2ec742f5a6bcf4c4d037', name: 'token7', holder: '0xf02d008e707755b4d855b201fd635dc56de327b3' },
  { address: '0x223a368ad0e7396165fc629976d77596a51f155c', name: 'token8', holder: '0x964809fd08ebc4574d3488a7ac68d204afa94eeb' },
  { address: '0xc654721fbf1f374fd9ffa3385bba2f4932a6af55', name: 'token9', holder: '0xf022e27f7aef570de2271ba108e4ec3d7900ee37' },
];

const CANDIDATE_SLOTS = [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 51, 52];

// ERC-7201: keccak256(abi.encode(uint256(keccak256("openzeppelin.storage.ERC20")) - 1)) & ~bytes32(uint256(0xff))
const ERC7201_BALANCE_BASE = '0x52c63247e1f47db19d5ce0460030c497f067ca4cebf71ba98eeadabe20bace00';

let rpcId = 1;

async function rpcCall(method: string, params: any[]): Promise<any> {
  const res = await fetch(RPC, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ jsonrpc: '2.0', method, params, id: rpcId++ }),
  });
  const json: any = await res.json();
  if (json.error) throw new Error(`RPC error: ${JSON.stringify(json.error)}`);
  return json.result;
}

function hexToBytes(hex: string): Uint8Array {
  const clean = hex.startsWith('0x') ? hex.slice(2) : hex;
  const bytes = new Uint8Array(clean.length / 2);
  for (let i = 0; i < bytes.length; i++) {
    bytes[i] = parseInt(clean.substr(i * 2, 2), 16);
  }
  return bytes;
}

function pad32(hex: string): string {
  const clean = hex.startsWith('0x') ? hex.slice(2) : hex;
  return clean.padStart(64, '0');
}

// keccak256(abi.encode(address, uint256(slot)))
// abi.encode pads both to 32 bytes
function computeMappingSlot(key: string, slot: number): string {
  const keyPadded = pad32(key);
  const slotPadded = pad32(slot.toString(16));
  const combined = keyPadded + slotPadded;
  const hash = keccak256(hexToBytes(combined));
  return '0x' + hash;
}

// For ERC-7201: keccak256(abi.encode(address)) at base + 0 offset
// The mapping is at the base slot, so: keccak256(abi.encode(key, base_slot_as_uint256))
function computeErc7201Slot(key: string, baseSlot: string): string {
  const keyPadded = pad32(key);
  const basePadded = baseSlot.startsWith('0x') ? baseSlot.slice(2) : baseSlot;
  const combined = keyPadded + basePadded;
  const hash = keccak256(hexToBytes(combined));
  return '0x' + hash;
}

async function getBalance(token: string, holder: string): Promise<string> {
  // balanceOf(address) = 0x70a08231
  const data = '0x70a08231' + pad32(holder);
  const result = await rpcCall('eth_call', [{ to: token, data }, 'latest']);
  return result;
}

async function getStorageAt(contract: string, slot: string): Promise<string> {
  return await rpcCall('eth_getStorageAt', [contract, slot, 'latest']);
}

async function findSlot(token: { address: string; name: string; holder: string }): Promise<{ address: string; slot: number; erc7201?: boolean } | null> {
  const balance = await getBalance(token.address, token.holder);
  const balanceBn = BigInt(balance);

  if (balanceBn === 0n) {
    console.log(`  ${token.name} (${token.address}): holder ${token.holder} has ZERO balance, skipping`);
    return null;
  }

  console.log(`  ${token.name} (${token.address}): holder balance = ${balanceBn}`);

  // Try standard slots
  for (const slot of CANDIDATE_SLOTS) {
    const storageKey = computeMappingSlot(token.holder, slot);
    const stored = await getStorageAt(token.address, storageKey);
    const storedBn = BigInt(stored);
    if (storedBn === balanceBn && balanceBn !== 0n) {
      console.log(`    FOUND: slot ${slot}`);
      return { address: token.address, slot };
    }
  }

  // Try ERC-7201
  const erc7201Key = computeErc7201Slot(token.holder, ERC7201_BALANCE_BASE);
  const erc7201Stored = await getStorageAt(token.address, erc7201Key);
  const erc7201Bn = BigInt(erc7201Stored);
  if (erc7201Bn === balanceBn && balanceBn !== 0n) {
    console.log(`    FOUND: ERC-7201 (base=${ERC7201_BALANCE_BASE})`);
    return { address: token.address, slot: 0, erc7201_base: ERC7201_BALANCE_BASE } as any;
  }

  // Try Vyper-style: keccak256(slot . key) instead of keccak256(key . slot)
  for (const slot of CANDIDATE_SLOTS) {
    const slotPadded = pad32(slot.toString(16));
    const keyPadded = pad32(token.holder);
    const combined = slotPadded + keyPadded;
    const hash = '0x' + keccak256(hexToBytes(combined));
    const stored = await getStorageAt(token.address, hash);
    const storedBn = BigInt(stored);
    if (storedBn === balanceBn && balanceBn !== 0n) {
      console.log(`    FOUND: Vyper-style slot ${slot} (keccak256(slot, key))`);
      return { address: token.address, slot };
    }
  }

  console.log(`    NOT FOUND in standard slots`);
  return null;
}

async function main() {
  console.log('Finding ERC20 balance storage slots...\n');

  const results: any[] = [];

  for (const token of TOKENS) {
    const result = await findSlot(token);
    if (result) {
      results.push(result);
    }
  }

  console.log('\n=== RESULTS ===');
  console.log(JSON.stringify(results, null, 2));
}

main().catch(console.error);
