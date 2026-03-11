import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";

const TRANSFER_TOPIC = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef";
const DEPOSIT_TOPIC = "0xe1fffcc4923d04b559f4d29a8bfc6cda04eb5b0d3c460751c2402c5c5cc9109c";
const WITHDRAWAL_TOPIC = "0x7fcf532c15f0a6db0bd6d0e038bea71d30d808c7d98cb3bf7268a95bf5081b65";

const KNOWN_TOKENS: Record<string, string> = {
  "0xb31f66aa3c1e785363f0875a1b74e27b85fd66c7": "WAVAX",
  "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e": "USDC",
  "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7": "USDt",
  "0x49d5c2bdffac6ce2bfdb6640f4f80f226bc10bab": "WETH.e",
  "0x2b2c81e08f1af8835a78bb2a90ae924ace0ea4be": "sAVAX",
  "0xa7d7079b0fead91f3e65f86e8915cb59c1a4c664": "USDC.e",
  "0xc7198437980c041c805a1edcba50c1ce5db95118": "USDt.e",
  "0x3fe4902b275caf603c46c81f3d921bb8515b5bc0": "0x3fe490",
  "0x0000000000000000000000000000000000000000": "ZERO",
  "0x4c4af8dbc524681930a27b2f1af5bcc8062e6fb7": "WooFi_Router",
  "0x5520385bfcf07ec87c4c53a7d8d65595dff69fa4": "WooPP",
  "0x45a62b090df48243f12a21897e7ed91863e2c86b": "LFJ_Router",
  "0x06380c0e0912312b5150364b9dc4542ba0dbbc85": "V4_PoolMgr",
  "0xa02ec3ba8d17887567672b2cdcaf525534636ea0": "Algebra_Pool",
  "0xa40b49418c758ea0faa5d27662320adc7da16127": "LFJ_V1_Pool",
  "0x41100c6d2c6920b10d12cd8d59c8a9aa2ef56fc7": "Algebra_Pool2",
  "0x6cd2c4c74125a6ee1999a061b1cea9892e331339": "VaporDex_Pool",
};
function tok(a: string) { return KNOWN_TOKENS[a.toLowerCase()] ?? a.slice(0, 10); }

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

function collectLogs(call: any, depth = 0): Array<{address: string, topics: string[], data: string, depth: number, type?: string, callTo?: string}> {
  const logs: any[] = [];
  if (call.logs) {
    for (const log of call.logs) {
      logs.push({ address: log.address.toLowerCase(), topics: log.topics, data: log.data, depth });
    }
  }
  if (call.calls) {
    for (const sub of call.calls) logs.push(...collectLogs(sub, depth + 1));
  }
  return logs;
}

async function traceIt(hash: string, block: number) {
  console.log(`\n${"=".repeat(70)}\n=== TX ${hash.slice(0,10)} at block ${block} ===\n${"=".repeat(70)}`);
  const tx = await client.getTransaction({ hash: hash as Hex });
  const parentBlock = `0x${(BigInt(block) - 1n).toString(16)}`;
  
  const trace: any = await client.request({
    method: "debug_traceCall" as any,
    params: [{
      from: tx.from, to: tx.to, data: tx.input,
      value: tx.value ? `0x${tx.value.toString(16)}` : undefined,
      gas: "0x1C9C380",
    }, parentBlock, { tracer: "callTracer", tracerConfig: { withLog: true } }] as any,
  });

  const logs = collectLogs(trace);
  
  console.log("\nAll Transfer/Deposit/Withdrawal events:");
  let idx = 0;
  for (const log of logs) {
    if (log.topics[0] === TRANSFER_TOPIC && log.topics.length >= 3) {
      const token = tok(log.address);
      const from = tok(("0x" + log.topics[1].slice(26)).toLowerCase());
      const to = tok(("0x" + log.topics[2].slice(26)).toLowerCase());
      const amount = BigInt(log.data);
      console.log(`  [${idx}] Transfer ${token}: ${from} → ${to}  amt=${amount}`);
    }
    if (log.topics[0] === DEPOSIT_TOPIC) {
      const who = tok(("0x" + log.topics[1].slice(26)).toLowerCase());
      console.log(`  [${idx}] DEPOSIT(wrap AVAX→WAVAX) by ${who} at ${tok(log.address)}`);
    }
    if (log.topics[0] === WITHDRAWAL_TOPIC) {
      const who = tok(("0x" + log.topics[1].slice(26)).toLowerCase());
      console.log(`  [${idx}] WITHDRAWAL(unwrap WAVAX→AVAX) by ${who} at ${tok(log.address)}`);
    }
    idx++;
  }
  
  // Show call tree (abbreviated)
  console.log("\nCall tree:");
  function showCalls(call: any, indent = "") {
    const to = call.to?.toLowerCase() ?? "?";
    const type = call.type ?? "CALL";
    const sel = call.input?.slice(0, 10) ?? "";
    const val = call.value && call.value !== "0x0" ? ` value=${call.value}` : "";
    console.log(`${indent}${type} → ${tok(to)} (${to.slice(0,10)}) sel=${sel}${val}`);
    if (call.calls && indent.length < 12) {
      for (const sub of call.calls) showCalls(sub, indent + "  ");
    }
  }
  showCalls(trace);
}

async function main() {
  // TX 1
  await traceIt("0x2a67681c63a3ca743798e6a2138b588cda23051d34e5a6f028193798c7213430", 80170733);
  // TX 2
  await traceIt("0xdf1deea5f8696cc8b0d8564f2b82bd0dbf4b1f985d163028fd0f240c5b3c9236", 80163410);
  // TX 3
  await traceIt("0xf2c2d7db1532c3ae9d38650753f776ace15b287e6e8bb2d6ccf2496f11e7bc71", 80141929);
}
main().catch(console.error);
