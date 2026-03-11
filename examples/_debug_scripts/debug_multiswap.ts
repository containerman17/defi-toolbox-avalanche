import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

const TRANSFER_TOPIC = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef";
const MULTISWAP_POOL = "0x5f1e8ed8468232bab71eda9f4598bda3161f48ea";
const ZERO_ADDR = "0x0000000000000000000000000000000000000000";

function collectLogs(call: any): any[] {
  const logs: any[] = [];
  if (call.logs) {
    for (const log of call.logs) {
      logs.push({ address: (log.address as string).toLowerCase(), topics: log.topics, data: log.data });
    }
  }
  if (call.calls) {
    for (const sub of call.calls) logs.push(...collectLogs(sub));
  }
  return logs;
}

async function main() {
  const txHash = "0x47e00934c4c69c5f47f3b03b5f7ce3070c983205bab36e63b751504dd488a83d" as `0x${string}`;
  const block = 80127543;

  const tx = await client.getTransaction({ hash: txHash });

  const resp = await fetch("http://localhost:9650/ext/bc/C/rpc", {
    method: "POST",
    headers: {"Content-Type": "application/json"},
    body: JSON.stringify({
      jsonrpc: "2.0",
      method: "debug_traceCall",
      params: [{
        from: tx.from,
        to: tx.to,
        data: tx.input,
        value: "0x" + tx.value.toString(16),
        gas: "0x" + (tx.gas * 2n).toString(16),
      }, "0x" + (BigInt(block) - 1n).toString(16), {
        tracer: "callTracer",
        tracerConfig: { withLog: true }
      }],
      id: 1
    })
  });
  const result: any = await resp.json();

  if (result.error) {
    console.log("Trace error:", result.error);
    return;
  }

  const allLogs = collectLogs(result.result);

  const transfers: any[] = [];
  for (let i = 0; i < allLogs.length; i++) {
    const log = allLogs[i];
    if (log.topics[0] === TRANSFER_TOPIC && log.topics.length >= 3) {
      transfers.push({
        token: log.address,
        from: ("0x" + log.topics[1].slice(26)).toLowerCase(),
        to: ("0x" + log.topics[2].slice(26)).toLowerCase(),
        amount: BigInt(log.data),
        logIndex: i,
      });
    }
  }

  console.log("All transfers:");
  for (const t of transfers) {
    console.log(`  token=${t.token.slice(0, 10)} from=${t.from.slice(0, 10)} to=${t.to.slice(0, 10)} amt=${t.amount}`);
  }

  const multiswapIn = transfers.filter((t: any) => t.to === MULTISWAP_POOL && t.from !== ZERO_ADDR);
  const multiswapOut = transfers.filter((t: any) => t.from === MULTISWAP_POOL && t.to !== ZERO_ADDR);
  console.log(`\nMultiswap pool incoming: ${multiswapIn.length}`);
  console.log(`Multiswap pool outgoing: ${multiswapOut.length}`);
  if (multiswapIn.length > 0) console.log("  in:", multiswapIn[0].token, multiswapIn[0].amount.toString());
  if (multiswapOut.length > 0) console.log("  out:", multiswapOut[0].token, multiswapOut[0].amount.toString());
}
main().catch(console.error);
