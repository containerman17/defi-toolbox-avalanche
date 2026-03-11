// Find ERC20 balance storage slots for specified tokens
// Uses batched JSON-RPC calls and js-sha3

import pkg from 'js-sha3';
const { keccak256 } = pkg;

const RPC = 'http://localhost:9650/ext/bc/C/rpc';

const TOKENS = [
  '0xacc95afa65768aa74044e6f6e267ad6417cd3e55',
  '0xb9c188bc558a82a1ee9e75ae0857df443f407632',
  '0x87202a2402414f4f58c52764faa7b015d104be82',
  '0x83a283641c6b4df383bcddf807193284c84c5342',
  '0x7698a5311da174a95253ce86c21ca7272b9b05f8',
  '0x5a15bdcf9a3a8e799fa4381e666466a516f2d9c8',
  '0x0f669808d88b2b0b3d23214dcd2a1cc6a8b1b5cd',
  '0x03d1b16e0550aba415e768f7c564313c15ebc3ed',
  '0x997ddaa07d716995de90577c123db411584e5e46',
  '0x108468885eba2932901d409af6bf97bf9c3eda50',
  '0xa813d175675c7f19bb7fd541f5ad1bcaf2117fe7',
  '0x69e24b22eb0fe616e2478fb5e4773d3275792535',
  '0x42069000770c482fed048e1da03a5f82773abd69',
  '0x211cc4dd073734da055fbf44a2b4667d5e5fe5d2',
  '0xfc064f65c11c11194332c53231935699be40af19',
  '0xfb98b335551a418cd0737375a2ea0ded62ea213b',
  '0xfab550568c688d5d8a52c7d794cb93edc26ec0ec',
  '0xfa0008d21919a27b80a2a59e197f5392bb4f048c',
  '0xf891214fdcf9cdaa5fdc42369ee4f27f226adad6',
  '0xf39f9671906d8630812f9d9863bbef5d523c84ab',
  '0xe98e1dd1541f604e47db990336f687e9c6fa56d1',
  '0xdf788ad40181894da035b827cdf55c523bf52f67',
  '0xce9cb2185f9ad5cfb82fc9e6fb81fb07b29bd414',
  '0xce1bffbd5374dac86a2893119683f4911a2f7814',
  '0xc212987801bc64c898bf9356733feb53a56f3ae2',
  '0xc06e17bdc3f008f4ce08d27d364416079289e729',
  '0xb0cf73accd48febea2bb543a91cfb0d16e7f96a2',
  '0xacc37335ad138c1d60ea7a805ac0558fa49c5317',
  '0xa77d05fd853af120cd4db48e73498e0cabd3f628',
  '0xa5e2cfe48fe8c4abd682ca2b10fcaafe34b8774c',
  '0x9bef43873393b7b25c8aa60f6e5118ab1fff4baa',
  '0x8cf71b4f445a47f234a0dc61034708a4087bead0',
  '0x8c8d2a7d8d9cf26f5ee1bbfc0ba56e93f4a4a7ac',
  '0x8877f4605ddfbc05cf37685de74e9f6232080a09',
  '0x810a45ecaa8077c8d1fdbfe03e0ca96015cb79e2',
  '0x7864e3bde05aa90ada055bfec4165f65f4fba7c5',
  '0x747021d5f5ad7d317b766674b25b8cfc81635f6e',
  '0x742dc16f79adb876a707ad2c973d216c6922d55b',
  '0x694207a9f708355ee3119f11e55bc5c0b1845ba2',
  '0x66fa127c1858d8f7346f79d1958450acf1469ddb',
  '0x502580fc390606b47fc3b741d6d49909383c28a9',
  '0x4bdca1feff104b22061bc91451c4bd9312d82aae',
  '0x444444444444c1a66f394025ac839a535246fcc8',
  '0x42006ab57701251b580bdfc24778c43c9ff589a1',
  '0x37b6b53f2c7048f260cea145ffa28fa0ff800fe9',
  '0x340fe1d898eccaad394e2ba0fc1f93d27c7b717a',
  '0x217de0ece28a34876cf4f6e2a830a552a1d46b06',
  '0x1db749847c4abb991d8b6032102383e6bfd9b1c7',
  '0x1c7c53aa86b49a28c627b6450091998e447a42f9',
  '0x18e3605b13f10016901eac609b9e188cf7c18973',
  '0x133879524ddb38582cf0b93d10adb789601ff397',
  '0x0ffd07d4ce09bd72a71d2dc6ca731575f2e5b408',
  '0x09d156f209e0c54d0365d6bb05f8a048649f2542',
];

const CANDIDATE_SLOTS = [0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,51,52,101,151,208];
const ERC7201_BALANCE_BASE = '52c63247e1f47db19d5ce0460030c497f067ca4cebf71ba98eeadabe20bace00';

const COMMON_HOLDERS = [
  '0x000000000000000000000000000000000000dEaD',
  '0x60aE616a2155Ee3d9A68541Ba4544862310933d4', // TraderJoe Router
  '0xdef171fe48cf0115b1d80b88dc8eab59176fee57', // Paraswap
  '0x1111111254EEB25477B68fb85Ed929f73A960582', // 1inch
  '0x9702230A8Ea53601f5cD2dc00fDBc13d4dF4A8c7', // USDt
  '0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E', // USDC
  '0x6e84a6216eA6dACC71eE8E6b0a5B7322EEbC0fDd', // JOE
  '0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7', // WAVAX
];

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
  json.sort((a: any, b: any) => a.id - b.id);
  return json.map((j: any) => j.result ?? null);
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

function mappingSlot(key: string, slot: string): string {
  return '0x' + keccak256(hexToBytes(pad32(key) + pad32(slot)));
}

function vyperMappingSlot(key: string, slot: string): string {
  return '0x' + keccak256(hexToBytes(pad32(slot) + pad32(key)));
}

async function findHolderFromLogs(token: string): Promise<string | null> {
  const transferTopic = '0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef';
  try {
    const blockHex = await rpcCall('eth_blockNumber', []);
    const block = BigInt(blockHex);
    // Try last 500k blocks
    const fromBlock = '0x' + (block - 500000n > 0n ? (block - 500000n).toString(16) : '0');
    const logs = await rpcCall('eth_getLogs', [{
      address: token,
      topics: [transferTopic],
      fromBlock,
      toBlock: 'latest',
    }]);
    if (logs && logs.length > 0) {
      const candidates: string[] = [];
      for (let i = logs.length - 1; i >= Math.max(0, logs.length - 30); i--) {
        if (logs[i].topics && logs[i].topics.length >= 3) {
          const to = '0x' + logs[i].topics[2].slice(26);
          const from = '0x' + logs[i].topics[1].slice(26);
          if (to !== '0x0000000000000000000000000000000000000000') candidates.push(to);
          if (from !== '0x0000000000000000000000000000000000000000') candidates.push(from);
        }
      }
      const unique = [...new Set(candidates)];
      const balCalls = unique.slice(0, 20).map(h => ({
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
  } catch {}

  // Try wider range
  try {
    const blockHex = await rpcCall('eth_blockNumber', []);
    const block = BigInt(blockHex);
    const fromBlock = '0x' + (block - 2000000n > 0n ? (block - 2000000n).toString(16) : '0');
    const logs = await rpcCall('eth_getLogs', [{
      address: token,
      topics: [transferTopic],
      fromBlock,
      toBlock: 'latest',
    }]);
    if (logs && logs.length > 0) {
      const candidates: string[] = [];
      for (let i = logs.length - 1; i >= Math.max(0, logs.length - 30); i--) {
        if (logs[i].topics && logs[i].topics.length >= 3) {
          const to = '0x' + logs[i].topics[2].slice(26);
          if (to !== '0x0000000000000000000000000000000000000000') candidates.push(to);
        }
      }
      const unique = [...new Set(candidates)];
      const balCalls = unique.slice(0, 20).map(h => ({
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
  } catch {}

  return null;
}

async function findSlotForToken(token: string, holder: string, balance: bigint, extraSlots?: number[]): Promise<{slot: string; style: string} | null> {
  const slots = extraSlots ? [...CANDIDATE_SLOTS, ...extraSlots] : CANDIDATE_SLOTS;
  const calls: {method: string; params: any[]}[] = [];
  const slotLabels: {slot: number; style: string}[] = [];

  // Solidity-style
  for (const s of slots) {
    calls.push({ method: 'eth_getStorageAt', params: [token, mappingSlot(holder, s.toString(16)), 'latest'] });
    slotLabels.push({ slot: s, style: 'solidity' });
  }
  // Vyper-style
  for (const s of slots) {
    calls.push({ method: 'eth_getStorageAt', params: [token, vyperMappingSlot(holder, s.toString(16)), 'latest'] });
    slotLabels.push({ slot: s, style: 'vyper' });
  }
  // ERC-7201
  calls.push({ method: 'eth_getStorageAt', params: [token, mappingSlot(holder, ERC7201_BALANCE_BASE), 'latest'] });
  slotLabels.push({ slot: -1, style: 'erc7201' });

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
        const stored = BigInt(results[i]);
        if (stored === balance && balance > 0n) {
          return { slot: slotLabels[i].slot.toString(), style: slotLabels[i].style };
        }
        // Check packed storage: balance might be in lower bits
        if (stored > 0n && stored !== balance) {
          const mask128 = (1n << 128n) - 1n;
          if ((stored & mask128) === balance && balance > 0n) {
            return { slot: slotLabels[i].slot.toString(), style: slotLabels[i].style + '_packed' };
          }
        }
      } catch {}
    }
  }
  return null;
}

async function main() {
  const results: any[] = [];
  const notFound: string[] = [];

  // Step 1: Find holders via common addresses + token itself
  type HolderEntry = { token: string; holder: string };
  const holdersToTry: HolderEntry[] = [];
  const tokenHolderCounts = new Map<string, number>();

  for (const t of TOKENS) {
    const holders = [...COMMON_HOLDERS, t];
    tokenHolderCounts.set(t, holders.length);
    for (const h of holders) holdersToTry.push({ token: t, holder: h });
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

  const tokenHolders = new Map<string, { holder: string; balance: bigint }>();
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

  // Step 2: Find holders from Transfer logs for tokens without one
  const needLogs = TOKENS.filter(t => !tokenHolders.has(t));
  if (needLogs.length > 0) {
    console.error(`Finding holders from Transfer logs for ${needLogs.length} tokens...`);
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

  // Step 3: Find slots
  const EXTRA_SLOTS = [
    21,22,23,24,25,26,27,28,29,30,31,32,33,34,35,36,37,38,39,40,
    41,42,43,44,45,46,47,48,49,50,53,54,55,56,57,58,59,60,
    100,102,103,104,105,150,152,153,200,201,202,203,204,205,206,207,209,210,
    256,257,258,259,260,300,350,400,500,516,517,518,519,520,1000,
  ];

  for (const token of TOKENS) {
    const info = tokenHolders.get(token);
    if (!info) {
      console.error(`${token}: no holder found`);
      notFound.push(token);
      continue;
    }

    // Try standard slots first
    let found = await findSlotForToken(token, info.holder, info.balance);
    if (!found) {
      // Try extended slots
      found = await findSlotForToken(token, info.holder, info.balance, EXTRA_SLOTS);
    }
    if (!found) {
      // Try with a different holder from logs
      const holder2 = await findHolderFromLogs(token);
      if (holder2 && holder2.toLowerCase() !== info.holder.toLowerCase()) {
        const bal2Result = await rpcCall('eth_call', [{ to: token, data: '0x70a08231' + pad32(holder2) }, 'latest']);
        if (bal2Result) {
          try {
            const bn2 = BigInt(bal2Result);
            if (bn2 > 0n) {
              found = await findSlotForToken(token, holder2, bn2, EXTRA_SLOTS);
            }
          } catch {}
        }
      }
    }

    if (found) {
      if (found.style === 'erc7201') {
        const entry: any = {
          address: token,
          slot: 0,
          erc7201_base: '0x' + ERC7201_BALANCE_BASE,
          erc7201_allowance: '0x' + ERC7201_BALANCE_BASE.replace(/00$/, '01'),
        };
        results.push(entry);
        console.log(JSON.stringify(entry));
      } else {
        const slotNum = parseInt(found.slot, 10);
        const entry: any = { address: token, slot: slotNum };
        if (found.style.includes('vyper')) entry.vyper = true;
        if (found.style.includes('packed')) entry.packed = true;
        results.push(entry);
        console.log(JSON.stringify(entry));
      }
    } else {
      console.error(`${token}: SLOT_NOT_FOUND (holder=${info.holder}, balance=${info.balance})`);
      notFound.push(token);
    }
  }

  console.error(`\n--- Summary ---`);
  console.error(`Found: ${results.length}/${TOKENS.length}`);
  if (notFound.length > 0) {
    console.error(`Not found: ${notFound.length}`);
    for (const t of notFound) console.error(`  ${t}`);
  }
}

main().catch(console.error);
