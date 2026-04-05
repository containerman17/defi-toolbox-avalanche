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
console.log('Connected. Waiting for first block...');

await new Promise(resolve => {
  globalThis.subscribeBlocks((block, ts) => {
    console.log(`Block ${block}`);
    resolve();
  });
});

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

console.log('\n' + [
  'Token'.padEnd(8),
  'Buy'.padStart(18),
  'ms'.padStart(7),
  'Sell→USDC'.padStart(12),
  'Spread'.padStart(8),
  '│ SplitBuy'.padStart(20),
  'SplitSell'.padStart(12),
  'SplitSprd'.padStart(10),
  'Δ'.padStart(8),
].join(''));
console.log('─'.repeat(103));

while (true) {
  for (const tok of TOKENS) {
    // Single path: buy 100 USDC → token
    const t0 = performance.now();
    const buy = await globalThis.quote(USDC, tok.addr, AMOUNT_100_USDC, true);
    const buyMs = performance.now() - t0;
    if (!buy.forward) continue;
    const buyAmount = buy.forward.amountOut;
    if (buyAmount === '0' || !buyAmount) continue;
    quoteCount++;
    totalMs += buyMs;

    // Single path: sell token → USDC
    const sell = await globalThis.quote(tok.addr, USDC, buyAmount, true);
    if (!sell.forward || sell.forward.amountOut === '0') continue;
    quoteCount++;

    const sellBack = sell.forward.amountOut;
    const spreadPct = (Number(100000000n - BigInt(sellBack)) / 1000000).toFixed(2);

    // Split: use split buy amount, sell with split
    let splitBuyStr = '—';
    let splitSellStr = '—';
    let splitSpreadStr = '—';
    let deltaStr = '';

    if (buy.split && buy.split.amountOut !== '0') {
      splitBuyStr = formatToken(buy.split.amountOut, tok.decimals);

      // Sell the split buy amount back
      const splitSell = await globalThis.quote(tok.addr, USDC, buy.split.amountOut, true);
      quoteCount++;

      if (splitSell.split && splitSell.split.amountOut !== '0') {
        splitSellStr = formatUSDC(splitSell.split.amountOut);
        const splitSpread = (Number(100000000n - BigInt(splitSell.split.amountOut)) / 1000000).toFixed(2);
        splitSpreadStr = splitSpread + '%';
        const delta = parseFloat(spreadPct) - parseFloat(splitSpread);
        if (Math.abs(delta) >= 0.005) {
          deltaStr = (delta > 0 ? '-' : '+') + Math.abs(delta).toFixed(2) + '%';
        } else {
          deltaStr = '=';
        }
      } else if (splitSell.forward && splitSell.forward.amountOut !== '0') {
        splitSellStr = formatUSDC(splitSell.forward.amountOut);
        const splitSpread = (Number(100000000n - BigInt(splitSell.forward.amountOut)) / 1000000).toFixed(2);
        splitSpreadStr = splitSpread + '%';
        const delta = parseFloat(spreadPct) - parseFloat(splitSpread);
        if (Math.abs(delta) >= 0.005) {
          deltaStr = (delta > 0 ? '-' : '+') + Math.abs(delta).toFixed(2) + '%';
        } else {
          deltaStr = '=';
        }
      }
    }

    console.log([
      tok.symbol.padEnd(8),
      formatToken(buyAmount, tok.decimals).padStart(18),
      `${buyMs.toFixed(0)}ms`.padStart(7),
      formatUSDC(sellBack).padStart(12),
      (spreadPct + '%').padStart(8),
      ('│ ' + splitBuyStr).padStart(20),
      splitSellStr.padStart(12),
      splitSpreadStr.padStart(10),
      deltaStr.padStart(8),
    ].join(''));
  }

  const avg = (totalMs / quoteCount).toFixed(0);
  console.log(`  [${quoteCount} quotes, avg ${avg} ms]\n`);
}
