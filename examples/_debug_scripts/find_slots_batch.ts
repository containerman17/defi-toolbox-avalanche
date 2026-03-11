// Find ERC20 balance storage slots for 33 tokens by probing storage
// Uses batched JSON-RPC calls for efficiency

import pkg from 'js-sha3';
const { keccak256 } = pkg;

const RPC = 'http://localhost:9650/ext/bc/C/rpc';

const TOKENS = [
  '0x009e245517fe420ad1afcf597f10910699a2c3a5',
  '0x0b5cb1b4dcde84e91cd3e26622114ec03843d102',
  '0x108468885eba2932901d409af6bf97bf9c3eda50',
  '0x1aec00b34b8fe66c7ec9bddef6ec989d0aeaf718',
  '0x386905718368aded7e544a40ca2bd60ceb4d28b0',
  '0x4a5bb433132b7e7f75d6a9a3e4136bb85ce6e4d5',
  '0x4d6ec47118f807ace03d3b3a4ee6aa96cb2ab677',
  '0x5cb33791453c4b6da99ff620a25031e784328e79',
  '0x6163b200cf8a9afba3e21d2c0e642e4b8dbba5e2',
  '0x7e5e413223c9d6e50adf090686c1dc034c9b2dd8',
  '0x874d79f599ab9864edef8fede48d701057a2c544',
  '0x8c6d71a0300c76dc5cfdbc2d37c85bca979ec683',
  '0x957f759aa6f494e219206f5202cecd17e98a901a',
  '0x9b58a88ed14e6897fa2c1d0614c5f6ba3df82401',
  '0xc51fd8db75e8a57b2041cb914fef01f8121cba25',
  '0xdbc5192a6b6ffee7451301bb4ec312f844f02b4a',
  '0xe29f8b925efef6e8b2c73bd468c559389c9f641d',
  '0xe3f3ef63f193f001d72bb623ca6ece3e71451d3f',
  '0xea325ccc2b98dd04d947a9e68c27c8dae6ad0f7e',
  '0xefa670f00447b13d92b639e06829079ed16498ab',
  '0xfa3a1e02d025dfb5730358e16a924489054fc7c4',
  '0x180af87b47bf272b2df59dccf2d76a6eafa625bf',
];

const CANDIDATE_SLOTS = [0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,51,52,101,151,208];
const ERC7201_BALANCE_BASE = '52c63247e1f47db19d5ce0460030c497f067ca4cebf71ba98eeadabe20bace00';

// Common addresses to try as holders
const COMMON_HOLDERS = [
  '0x000000000000000000000000000000000000dEaD',
  '0x18556DA13313f3532c54711497A8FedAC273220E',
  // token itself will be added dynamically
];

// Known holders from Routescan Transfer logs for tokens that are hard to find
const KNOWN_HOLDERS: Record<string, string[]> = {
  '0x700103766c23fb8da956caf94c756b90106eeb41': ['0x0f2a670024c28c7e1a3a9a61ffebf9e60cc5e9ee'],
  '0xc5718c4c91e9d051b082e02b1138ab5b6a01dcd1': ['0xe3b8b0f6716e10b4b13c54ff8aae08a0b02a0ea8'],
  '0x0256b279d973c8d687264ac3eb36be09232d4474': ['0x768f3c20adb66fbbf052eb0401dad12457391fa8', '0xdfb8459d4c567a8d45a6784ef8f451dc59d20cda'],
  '0x03e8d118a1864c7dc53bf91e007ab7d91f5a06fa': ['0x7113e5c3357730af090bd2e55120d21d095b40f1'],
  '0x91870b9c25c06e10bcb88bdd0f7b43a13c2d7c41': ['0x5d2588d514a27942933bcb081d01343245a4b1c8'],
  '0x13af0fe9eb35e91758b467f95cbc78e16fdd8b6b': ['0x03173e114410dd522ed3b8263f56693796fed8f2'],
  '0xd7ef0b8763a9053c051f1d86b317922243a03099': ['0x816224dfe8c2434ad61beee4000d197391adef50'],
  '0xf3dd4e0a1db7c5dcbf3b225698cb6a916aeb24d9': ['0xa0ce71b4ed736f02c269498f870dcbb0c8ec257c', '0x6e6682b9001e13a8665b8c8d6a57391d88f0759f'],
};

let rpcId = 1;

async function rpcBatch(calls: {method: string; params: any[]}[]): Promise<any[]> {
  if (calls.length === 0) return [];
  const batch = calls.map(c => ({ jsonrpc: '2.0', method: c.method, params: c.params, id: rpcId++ }));
  const res = await fetch(RPC, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(batch),
  });
  const json: any[] = await res.json();
  json.sort((a, b) => a.id - b.id);
  return json.map(j => j.result ?? null);
}

async function rpcCall(method: string, params: any[]): Promise<any> {
  const res = await fetch(RPC, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ jsonrpc: '2.0', method, params, id: rpcId++ }),
  });
  const json: any = await res.json();
  return json.result ?? null;
}

function hexToBytes(hex: string): Uint8Array {
  const clean = hex.startsWith('0x') ? hex.slice(2) : hex;
  const bytes = new Uint8Array(clean.length / 2);
  for (let i = 0; i < bytes.length; i++) bytes[i] = parseInt(clean.substr(i * 2, 2), 16);
  return bytes;
}

function pad32(hex: string): string {
  const clean = hex.startsWith('0x') ? hex.slice(2) : hex;
  return clean.padStart(64, '0');
}

// keccak256(abi.encode(address, uint256(slot))) - Solidity style
function mappingSlot(key: string, slot: string): string {
  return '0x' + keccak256(hexToBytes(pad32(key) + pad32(slot)));
}

// keccak256(abi.encode(uint256(slot), address)) - Vyper style
function vyperMappingSlot(key: string, slot: string): string {
  return '0x' + keccak256(hexToBytes(pad32(slot) + pad32(key)));
}

async function findHolderFromLogs(token: string): Promise<string | null> {
  const transferTopic = '0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef';
  // Try progressively smaller block ranges
  const blockRanges: [string, string][] = [['0x0', 'latest']];
  try {
    const blockHex = await rpcCall('eth_blockNumber', []);
    const block = BigInt(blockHex);
    blockRanges.push(['0x' + (block - 500000n).toString(16), 'latest']);
    blockRanges.push(['0x' + (block - 50000n).toString(16), 'latest']);
  } catch {}

  for (const [fromBlock, toBlock] of blockRanges) {
    try {
      const logs = await rpcCall('eth_getLogs', [{
        address: token,
        topics: [transferTopic],
        fromBlock,
        toBlock,
      }]);
      if (logs && logs.length > 0) {
        const candidates: string[] = [];
        for (let i = logs.length - 1; i >= Math.max(0, logs.length - 20); i--) {
          if (logs[i].topics.length >= 3) {
            const to = '0x' + logs[i].topics[2].slice(26);
            const from = '0x' + logs[i].topics[1].slice(26);
            if (to !== '0x0000000000000000000000000000000000000000') candidates.push(to);
            if (from !== '0x0000000000000000000000000000000000000000') candidates.push(from);
          }
        }
        if (candidates.length > 0) {
          const unique = [...new Set(candidates)];
          const balCalls = unique.slice(0, 15).map(h => ({
            method: 'eth_call',
            params: [{ to: token, data: '0x70a08231' + pad32(h) }, 'latest'],
          }));
          const bals = await rpcBatch(balCalls);
          for (let i = 0; i < bals.length; i++) {
            if (bals[i]) {
              try { if (BigInt(bals[i]) > 0n) return unique[i]; } catch {}
            }
          }
        }
      }
    } catch {}
  }
  return null;
}

async function findSlotForToken(token: string, holder: string, balance: bigint, extraSlots?: number[]): Promise<string | null> {
  const slots = extraSlots ? [...CANDIDATE_SLOTS, ...extraSlots] : CANDIDATE_SLOTS;
  const calls: {method: string; params: any[]}[] = [];
  const slotLabels: {slot: number; style: string}[] = [];

  for (const s of slots) {
    const sHex = s.toString(16);
    calls.push({ method: 'eth_getStorageAt', params: [token, mappingSlot(holder, sHex), 'latest'] });
    slotLabels.push({ slot: s, style: 'solidity' });
  }
  for (const s of slots) {
    const sHex = s.toString(16);
    calls.push({ method: 'eth_getStorageAt', params: [token, vyperMappingSlot(holder, sHex), 'latest'] });
    slotLabels.push({ slot: s, style: 'vyper' });
  }
  // ERC-7201
  calls.push({ method: 'eth_getStorageAt', params: [token, mappingSlot(holder, ERC7201_BALANCE_BASE), 'latest'] });
  slotLabels.push({ slot: -1, style: 'erc7201' });

  // Send in chunks
  const CHUNK = 100;
  const results: any[] = [];
  for (let i = 0; i < calls.length; i += CHUNK) {
    const chunk = calls.slice(i, i + CHUNK);
    const res = await rpcBatch(chunk);
    results.push(...res);
  }

  for (let i = 0; i < results.length; i++) {
    if (results[i]) {
      try {
        if (BigInt(results[i]) === balance) {
          const label = slotLabels[i];
          if (label.style === 'erc7201') return 'ERC7201';
          return String(label.slot);
        }
      } catch {}
    }
  }
  return null;
}

async function main() {
  const results: Map<string, string> = new Map();

  // Step 1: Get balances for all tokens with common holders + known holders (batch)
  type HolderEntry = { token: string; holder: string };
  const holdersToTry: HolderEntry[] = [];
  const tokenHolderCounts: Map<string, number> = new Map();

  for (const t of TOKENS) {
    const holders = [...COMMON_HOLDERS, t]; // common + token itself
    const known = KNOWN_HOLDERS[t.toLowerCase()] || [];
    holders.push(...known);
    tokenHolderCounts.set(t, holders.length);
    for (const h of holders) {
      holdersToTry.push({ token: t, holder: h });
    }
  }

  // Batch balanceOf calls
  const balCalls = holdersToTry.map(({ token, holder }) => ({
    method: 'eth_call',
    params: [{ to: token, data: '0x70a08231' + pad32(holder) }, 'latest'],
  }));

  const CHUNK = 100;
  const allBalances: (string | null)[] = [];
  for (let i = 0; i < balCalls.length; i += CHUNK) {
    const chunk = balCalls.slice(i, i + CHUNK);
    const res = await rpcBatch(chunk);
    allBalances.push(...res);
  }

  // Find best holder per token
  const tokenHolders: Map<string, { holder: string; balance: bigint }> = new Map();
  let idx = 0;
  for (const t of TOKENS) {
    const count = tokenHolderCounts.get(t)!;
    for (let hi = 0; hi < count; hi++) {
      const bal = allBalances[idx + hi];
      if (bal) {
        try {
          const bn = BigInt(bal);
          if (bn > 0n && !tokenHolders.has(t)) {
            tokenHolders.set(t, { holder: holdersToTry[idx + hi].holder, balance: bn });
          }
        } catch {}
      }
    }
    idx += count;
  }

  // Step 2: For tokens without a holder, find one from Transfer logs
  const needLogs = TOKENS.filter(t => !tokenHolders.has(t));
  if (needLogs.length > 0) {
    console.error(`Looking up holders from Transfer logs for ${needLogs.length} tokens...`);
    for (const token of needLogs) {
      const holder = await findHolderFromLogs(token);
      if (holder) {
        const balResult = await rpcCall('eth_call', [{ to: token, data: '0x70a08231' + pad32(holder) }, 'latest']);
        if (balResult) {
          try {
            const bn = BigInt(balResult);
            if (bn > 0n) tokenHolders.set(token, { holder, balance: bn });
          } catch {}
        }
      }
    }
  }

  // Step 3: Find slots for all tokens that have holders
  // Extended slots for hard-to-find tokens (covers Diamond, custom proxy patterns, etc.)
  const EXTRA_SLOTS = [
    21,22,23,24,25,26,27,28,29,30,31,32,33,34,35,36,37,38,39,40,
    41,42,43,44,45,46,47,48,49,50,
    53,54,55,56,57,58,59,60,100,102,103,104,105,
    150,152,153,154,155,200,201,202,203,204,205,206,207,209,210,
    256,257,258,259,260,300,350,400,500,1000,
  ];

  for (const token of TOKENS) {
    const info = tokenHolders.get(token);
    if (!info) {
      results.set(token, 'NOT_FOUND');
      continue;
    }
    // First try standard slots
    const slot = await findSlotForToken(token, info.holder, info.balance);
    if (slot) {
      results.set(token, slot);
      continue;
    }
    // If not found, try extended slots
    console.error(`  Trying extended slots for ${token} (holder=${info.holder}, bal=${info.balance})...`);
    const slot2 = await findSlotForToken(token, info.holder, info.balance, EXTRA_SLOTS);
    if (slot2) {
      results.set(token, slot2);
      continue;
    }
    // Try with a different holder from logs
    const holder2 = await findHolderFromLogs(token);
    if (holder2 && holder2.toLowerCase() !== info.holder.toLowerCase()) {
      const bal2Result = await rpcCall('eth_call', [{ to: token, data: '0x70a08231' + pad32(holder2) }, 'latest']);
      if (bal2Result) {
        try {
          const bn2 = BigInt(bal2Result);
          if (bn2 > 0n) {
            const slot3 = await findSlotForToken(token, holder2, bn2, EXTRA_SLOTS);
            if (slot3) {
              results.set(token, slot3);
              continue;
            }
          }
        } catch {}
      }
    }
    results.set(token, 'NOT_FOUND');
  }

  // Output results
  for (const token of TOKENS) {
    console.log(`${token} ${results.get(token)}`);
  }
}

main().catch(console.error);
