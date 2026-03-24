import { keccak256, pad, toHex } from "viem";
import { parsePools } from "../../../pool-collector/pools.ts";
import { ERC4626_VAULTS, generateBufferedEdges } from "../../../pool-collector/erc4626.ts";
import { buildGraph, findBestRoute } from "../../../pathfinder/index.ts";
import { createBrowserQuoter, ROUTER } from "../../../evm-quoter/sdk-browser.ts";

// ── Constants ────────────────────────────────────────────────────────

const WAVAX = "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7";
const USDC  = "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e";

const TOKENS: Record<string, { address: string; decimals: number; symbol: string; slot: number }> = {
  WAVAX: { address: WAVAX, decimals: 18, symbol: "AVAX", slot: 3 },
  USDC:  { address: USDC,  decimals: 6,  symbol: "USDC", slot: 9 },
};

// ── Logging ──────────────────────────────────────────────────────────

const logEl = document.getElementById("log")!;

function log(msg: string, cls = "log-status") {
  const line = document.createElement("div");
  line.className = cls;
  line.textContent = msg;
  logEl.appendChild(line);
  logEl.scrollTop = logEl.scrollHeight;
}

function clearLog() {
  logEl.innerHTML = "";
}

// ── State overrides ─────────────────────────────────────────────────

function balanceSlot(holder: string, slot: number): string {
  return keccak256(
    (pad(holder as `0x${string}`, { size: 32 }) +
     pad(toHex(slot), { size: 32 }).slice(2)) as `0x${string}`,
  );
}

function buildStateOverrides(bytecodeHex: string, tokenAmounts: Map<string, bigint>) {
  const overrides: Record<string, any> = {
    [ROUTER]: { code: "0x" + bytecodeHex },
  };

  for (const [tokenAddr, amount] of tokenAmounts) {
    const info = Object.values(TOKENS).find(t => t.address === tokenAddr);
    if (!info) continue;
    const slot = balanceSlot(ROUTER, info.slot);
    overrides[tokenAddr] = { stateDiff: { [slot]: pad(toHex(amount), { size: 32 }) } };
  }

  return overrides;
}

// ── Formatting ──────────────────────────────────────────────────────

function formatAmount(raw: bigint, decimals: number): string {
  const s = raw.toString().padStart(decimals + 1, "0");
  const int = s.slice(0, s.length - decimals) || "0";
  const frac = s.slice(s.length - decimals);
  return int + "." + frac.slice(0, 6);
}

function formatRoute(route: { pool: { address: string; providerName: string } }[]): string {
  return route.map(s => `${s.pool.providerName}(${s.pool.address.slice(0, 10)})`).join(" -> ");
}

// ── Quote flow ──────────────────────────────────────────────────────

async function runQuote() {
  const btn = document.getElementById("quoteBtn") as HTMLButtonElement;
  btn.disabled = true;
  btn.textContent = "Quoting...";
  clearLog();

  try {
    const fromKey = (document.getElementById("tokenIn") as HTMLSelectElement).value;
    const toKey = (document.getElementById("tokenOut") as HTMLSelectElement).value;
    const amountStr = (document.getElementById("amountIn") as HTMLInputElement).value;
    const stateServerUrl = (document.getElementById("stateServerUrl") as HTMLInputElement).value;

    if (fromKey === toKey) {
      log("Select different tokens", "log-error");
      return;
    }

    const tokenIn = TOKENS[fromKey];
    const tokenOut = TOKENS[toKey];
    const amountIn = BigInt(Math.round(parseFloat(amountStr) * 10 ** tokenIn.decimals));

    // 1. Load pools
    log("Fetching pools...");
    const poolsText = await fetch("/data/pools.txt").then(r => r.text());
    const { pools } = parsePools(poolsText);
    const poolList = [...pools.values()].slice(0, 1000);
    const buffered = generateBufferedEdges(poolList);
    const allPools = [...poolList, ...ERC4626_VAULTS, ...buffered];
    log(`Loaded ${poolList.length} pools + ${ERC4626_VAULTS.length} ERC4626 + ${buffered.length} buffered`);

    // 2. Build graph
    log("Building graph...");
    const graph = buildGraph(allPools);

    // 3. Build state overrides
    log("Loading router bytecode...");
    const bytecodeHex = (await fetch("/contracts/bytecode.hex").then(r => r.text())).trim();
    const overrides = buildStateOverrides(bytecodeHex, new Map([
      [WAVAX, 1000n * 10n ** 18n],
      [USDC, 1_000_000n * 10n ** 6n],
    ]));

    // 4. Create WASM quoter
    const quoter = await createBrowserQuoter({
      stateServerUrl,
      wasmUrl: "/bin/harness.wasm",
      onStatus: (msg) => log(msg),
    });

    // 5. Forward route: tokenIn → tokenOut
    log(`\nFinding best route: ${tokenIn.symbol} -> ${tokenOut.symbol}...`);
    const t0 = performance.now();
    const forward = await findBestRoute(quoter, graph, tokenIn.address, tokenOut.address, amountIn, overrides);

    if (!forward) {
      log("No route found!", "log-error");
      quoter.close();
      return;
    }

    const fwdMs = Math.round(performance.now() - t0);
    log(`Route:  ${formatRoute(forward.route)}`, "log-route");
    log(`Hops:   ${forward.route.length}`);
    log(`Input:  ${formatAmount(amountIn, tokenIn.decimals)} ${tokenIn.symbol}`);
    log(`Output: ${formatAmount(forward.amountOut, tokenOut.decimals)} ${tokenOut.symbol}`, "log-result");
    log(`EVM:    ${forward.stats.evmCalls} calls, ${forward.stats.cacheMisses} misses`);
    log(`Time:   ${fwdMs}ms`);

    // 6. Reverse route: tokenOut → tokenIn (roundtrip)
    log(`\nFinding return route: ${tokenOut.symbol} -> ${tokenIn.symbol}...`);
    const t1 = performance.now();
    const reverse = await findBestRoute(quoter, graph, tokenOut.address, tokenIn.address, forward.amountOut, overrides);

    if (!reverse) {
      log("No return route found!", "log-error");
      quoter.close();
      return;
    }

    const revMs = Math.round(performance.now() - t1);
    log(`Route:  ${formatRoute(reverse.route)}`, "log-route");
    log(`Hops:   ${reverse.route.length}`);
    log(`Input:  ${formatAmount(forward.amountOut, tokenOut.decimals)} ${tokenOut.symbol}`);
    log(`Output: ${formatAmount(reverse.amountOut, tokenIn.decimals)} ${tokenIn.symbol}`, "log-result");
    log(`EVM:    ${reverse.stats.evmCalls} calls, ${reverse.stats.cacheMisses} misses`);
    log(`Time:   ${revMs}ms`);

    // 7. Summary
    const returnRate = Number(reverse.amountOut * 10000n / amountIn) / 100;
    log(`\n${"─".repeat(45)}`, "log-status");
    log(`Roundtrip: ${formatAmount(amountIn, tokenIn.decimals)} ${tokenIn.symbol} -> ${formatAmount(forward.amountOut, tokenOut.decimals)} ${tokenOut.symbol} -> ${formatAmount(reverse.amountOut, tokenIn.decimals)} ${tokenIn.symbol}`, "log-result");
    log(`Return:    ${returnRate.toFixed(2)}%`, "log-result");
    log(`Total:     ${fwdMs + revMs}ms`, "log-status");

    quoter.close();
  } catch (err: any) {
    log(`Error: ${err.message}`, "log-error");
    console.error(err);
  } finally {
    btn.disabled = false;
    btn.textContent = "Quote";
  }
}

// ── Bind UI ─────────────────────────────────────────────────────────

document.getElementById("quoteBtn")!.addEventListener("click", runQuote);
