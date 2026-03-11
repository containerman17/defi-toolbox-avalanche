import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";

const WOMBAT_SWAP_TOPIC = "0x54787c404bb33c88e86f4baf88183a3b0141d0a848e6a9f7a13b66ae3a9b73d1";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

async function main() {
  // TX 1: check for Wombat swap events
  const tx = await client.getTransaction({ hash: "0x2a67681c63a3ca743798e6a2138b588cda23051d34e5a6f028193798c7213430" as Hex });
  const parentBlock = `0x${(80170733n - 1n).toString(16)}`;
  const trace: any = await client.request({
    method: "debug_traceCall" as any,
    params: [{
      from: tx.from, to: tx.to, data: tx.input,
      value: tx.value ? `0x${tx.value.toString(16)}` : undefined,
      gas: "0x1C9C380",
    }, parentBlock, { tracer: "callTracer", tracerConfig: { withLog: true } }] as any,
  });

  function collectLogs(call: any): any[] {
    const logs: any[] = [];
    if (call.logs) for (const log of call.logs) logs.push({ address: log.address.toLowerCase(), topics: log.topics, data: log.data });
    if (call.calls) for (const sub of call.calls) logs.push(...collectLogs(sub));
    return logs;
  }

  const logs = collectLogs(trace);
  console.log("Wombat Swap events in TX1:");
  for (const log of logs) {
    if (log.topics[0] === WOMBAT_SWAP_TOPIC) {
      console.log(`  address: ${log.address}`);
      console.log(`  data: ${log.data}`);
      // Parse: fromToken(address), toToken(address), fromAmount, toAmount
      const fromToken = ("0x" + log.data.slice(26, 66)).toLowerCase();
      const toToken = ("0x" + log.data.slice(90, 130)).toLowerCase();
      console.log(`  fromToken: ${fromToken}`);
      console.log(`  toToken: ${toToken}`);
    }
  }
  
  // TX 2: check for Wombat swap events
  const tx2 = await client.getTransaction({ hash: "0xdf1deea5f8696cc8b0d8564f2b82bd0dbf4b1f985d163028fd0f240c5b3c9236" as Hex });
  const parentBlock2 = `0x${(80163410n - 1n).toString(16)}`;
  const trace2: any = await client.request({
    method: "debug_traceCall" as any,
    params: [{
      from: tx2.from, to: tx2.to, data: tx2.input,
      value: tx2.value ? `0x${tx2.value.toString(16)}` : undefined,
      gas: "0x1C9C380",
    }, parentBlock2, { tracer: "callTracer", tracerConfig: { withLog: true } }] as any,
  });

  const logs2 = collectLogs(trace2);
  console.log("\nWombat Swap events in TX2:");
  for (const log of logs2) {
    if (log.topics[0] === WOMBAT_SWAP_TOPIC) {
      console.log(`  address: ${log.address}`);
      const fromToken = ("0x" + log.data.slice(26, 66)).toLowerCase();
      const toToken = ("0x" + log.data.slice(90, 130)).toLowerCase();
      console.log(`  fromToken: ${fromToken}, toToken: ${toToken}`);
    }
  }

  // TX 3: check for any special events 
  const tx3 = await client.getTransaction({ hash: "0xf2c2d7db1532c3ae9d38650753f776ace15b287e6e8bb2d6ccf2496f11e7bc71" as Hex });
  const parentBlock3 = `0x${(80141929n - 1n).toString(16)}`;
  const trace3: any = await client.request({
    method: "debug_traceCall" as any,
    params: [{
      from: tx3.from, to: tx3.to, data: tx3.input,
      value: tx3.value ? `0x${tx3.value.toString(16)}` : undefined,
      gas: "0x1C9C380",
    }, parentBlock3, { tracer: "callTracer", tracerConfig: { withLog: true } }] as any,
  });

  const logs3 = collectLogs(trace3);
  console.log("\nWombat Swap events in TX3:");
  let found3 = false;
  for (const log of logs3) {
    if (log.topics[0] === WOMBAT_SWAP_TOPIC) {
      console.log(`  address: ${log.address}`);
      const fromToken = ("0x" + log.data.slice(26, 66)).toLowerCase();
      const toToken = ("0x" + log.data.slice(90, 130)).toLowerCase();
      console.log(`  fromToken: ${fromToken}, toToken: ${toToken}`);
      found3 = true;
    }
  }
  if (!found3) console.log("  None found");
  
  // Check what 0x5f1e8ed8 does - look for any events it emits
  console.log("\nAll events from 0x5f1e8ed8 in TX3:");
  for (const log of logs3) {
    if (log.address === "0x5f1e8ed8468232bab71eda9f4598bda3161f48ea") {
      console.log(`  topic0: ${log.topics[0]}`);
      console.log(`  data len: ${log.data.length}`);
    }
  }
  
  // Check what WooPP does - it should swap WETH.e → USDt, but route says USDt output goes somewhere weird
  console.log("\nAll events from WooPP (0x5520385b) in TX3:");
  for (const log of logs3) {
    if (log.address === "0x5520385bfcf07ec87c4c53a7d8d65595dff69fa4") {
      console.log(`  topic0: ${log.topics[0]?.slice(0,10)}`);
    }
  }
}
main().catch(console.error);
