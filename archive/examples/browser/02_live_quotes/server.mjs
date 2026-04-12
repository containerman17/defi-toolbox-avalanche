#!/usr/bin/env node
// Node.js version of the live quotes demo. Runs the same WASM quoter
// (not native Go) against a local state server and prints a spread table.
//
// Usage: node server.mjs [state-server-ws-url] [pool-limit]
//   default: ws://localhost:7449/live, 2000 pools

import { readFileSync } from 'fs';
import { fileURLToPath } from 'url';
import { dirname, join } from 'path';

const __dirname = dirname(fileURLToPath(import.meta.url));

// ── Load Go WASM runtime ────────────────────────────────────────────
const wasmExecPath = '/usr/local/go/misc/wasm/wasm_exec.js';
const wasmExecCode = readFileSync(wasmExecPath, 'utf-8');

if (!globalThis.performance) {
  const { performance } = await import('perf_hooks');
  globalThis.performance = performance;
}
if (!globalThis.crypto) {
  const { webcrypto } = await import('crypto');
  globalThis.crypto = webcrypto;
}

new Function(wasmExecCode + '; globalThis.Go = Go;')();

// ── Build WASM from source (skip if already built) ──────────────────
import { execSync } from 'child_process';
import { existsSync } from 'fs';

const projectRoot = join(__dirname, '..', '..', '..');
const wasmPath = join(projectRoot, 'cmd', 'wasm-sdk', 'quoter.wasm');

if (!existsSync(wasmPath)) {
  console.log('Building WASM...');
  execSync('make build-wasm', { cwd: projectRoot, stdio: 'inherit' });
} else {
  console.log('Using existing WASM build.');
}

const go = new globalThis.Go();
const wasmBytes = readFileSync(wasmPath);
const { instance } = await WebAssembly.instantiate(wasmBytes, go.importObject);
go.run(instance);
await new Promise(r => setTimeout(r, 100));

// ── Connect to state server ─────────────────────────────────────────
const stateUrl = process.argv[2] || 'ws://localhost:7449/live';
const poolLimit = parseInt(process.argv[3]) || 2000;
console.log(`Connecting to ${stateUrl} (${poolLimit} pools)...`);
await globalThis.connect(stateUrl, poolLimit, 3);
// On /live, wait for first block. On /debug (frozen snapshot), start immediately.
const isDebug = stateUrl.includes('/debug/');
if (!isDebug) {
  console.log('Waiting for first block...');
  await new Promise(resolve => {
    globalThis.subscribeBlocks((block, ts) => {
      console.log(`Block ${block}`);
      resolve();
    });
  });
} else {
  console.log('Debug/snapshot mode — using frozen state.');
}

// ── Config (same as browser demo) ───────────────────────────────────
const USDC = '0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E';
const AMOUNT_100_USDC = '100000000';

const TOKENS = [
  { symbol: 'WAVAX',  addr: '0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7', decimals: 18 },
  { symbol: 'USDT',   addr: '0x9702230A8Ea53601f5cD2dc00fDBc13d4dF4A8c7', decimals: 6 },
  { symbol: 'WETH.e', addr: '0x49D5c2BdFfac6CE2BFdB6640F4F80f226bc10bAB', decimals: 18 },
  { symbol: 'BTC.b',  addr: '0x152b9d0FdC40C096757F570A51E494bd4b943E50', decimals: 8 },
  { symbol: 'sAVAX',  addr: '0x2b2C81e08f1Af8835a78Bb2A90AE924ACE0eA4bE', decimals: 18 },
  { symbol: 'EURC',   addr: '0xC891EB4CbdEFf6e073e859e987815Ed1505c2ACD', decimals: 6 },
  { symbol: 'stAVAX', addr: '0xa25EaF2906FA1a3a13EdAc9B9657108Af7B703e3', decimals: 18 },
  { symbol: 'LINK.e', addr: '0x5947BB275c521040051D82396192181b413227A3', decimals: 18 },
  { symbol: 'AAVE.e', addr: '0x63a72806098Bd3D9520cC43356dD78afe5D386D9', decimals: 18 },
  { symbol: 'JOE',    addr: '0x6e84a6216eA6dACC71eE8E6b0a5B7322EEbC0fDd', decimals: 18 },
];

// ── Helpers ──────────────────────────────────────────────────────────
function formatToken(amountStr, decimals) {
  const n = BigInt(amountStr);
  const d = BigInt(10) ** BigInt(decimals);
  const whole = n / d;
  const frac = (n % d).toString().padStart(decimals, '0').slice(0, Math.min(decimals, 6));
  return whole.toLocaleString() + '.' + frac;
}

function formatUSDC(amountStr) {
  const n = BigInt(amountStr);
  const whole = n / 1000000n;
  const frac = (n % 1000000n).toString().padStart(6, '0');
  return '$' + whole.toLocaleString() + '.' + frac.slice(0, 2);
}

// ── Quote loop ──────────────────────────────────────────────────────
console.log('\nWarming up...');
for (const tok of TOKENS) {
  await globalThis.quote(USDC, tok.addr, AMOUNT_100_USDC);
}

let quoteCount = 0;
let totalMs = 0;

// The quote() call with split=true computes both single-path and split on the
// SAME locked block. We compare forward.amountOut (single) vs split.amountOut
// from the same response — no block drift.
console.log('\n' + [
  'Token'.padEnd(8),
  'Single Buy'.padStart(18),
  'Split Buy'.padStart(18),
  'Δ buy'.padStart(14),
  '│ ms'.padStart(8),
  'legs'.padStart(5),
].join(''));
console.log('─'.repeat(71));

while (true) {
  for (const tok of TOKENS) {
    const t0 = performance.now();
    const res = await globalThis.quote(USDC, tok.addr, AMOUNT_100_USDC, true);
    const ms = performance.now() - t0;
    if (!res.forward) continue;

    const singleOut = res.forward.amountOut;
    if (singleOut === '0' || !singleOut) continue;
    quoteCount++;
    totalMs += ms;

    const singleStr = formatToken(singleOut, tok.decimals);
    let splitStr = '—';
    let deltaStr = '';
    let legsStr = '';

    if (res.split && res.split.amountOut !== '0') {
      splitStr = formatToken(res.split.amountOut, tok.decimals);
      legsStr = String(res.split.legs.length);

      const singleBI = BigInt(singleOut);
      const splitBI = BigInt(res.split.amountOut);
      const diff = splitBI - singleBI;

      if (diff > 0n) {
        // Split is better — show green
        const pct = Number(diff) / Number(singleBI) * 100;
        deltaStr = `+${pct.toFixed(4)}%`;
      } else if (diff < 0n) {
        // Split is worse — this should NEVER happen on same block
        const pct = Number(-diff) / Number(singleBI) * 100;
        deltaStr = `-${pct.toFixed(4)}% !!!`;
      } else {
        deltaStr = '=';
      }
    }

    console.log([
      tok.symbol.padEnd(8),
      singleStr.padStart(18),
      splitStr.padStart(18),
      deltaStr.padStart(14),
      ('│ ' + ms.toFixed(0) + 'ms').padStart(8),
      legsStr.padStart(5),
    ].join(''));
  }

  const avg = (totalMs / quoteCount).toFixed(0);
  console.log(`  [${quoteCount} quotes, avg ${avg} ms]\n`);
}
