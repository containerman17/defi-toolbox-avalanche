import { createPublicClient, http, type Hex } from "viem";
import { avalanche } from "viem/chains";

const client = createPublicClient({ chain: avalanche, transport: http("http://localhost:9650/ext/bc/C/rpc") });

async function getAddrs(hash: string, block: number) {
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
  
  const addrs = new Set<string>();
  function collect(call: any) {
    if (call.to) addrs.add(call.to.toLowerCase());
    if (call.calls) for (const sub of call.calls) collect(sub);
  }
  collect(trace);
  return addrs;
}

async function main() {
  const a1 = await getAddrs("0x2a67681c63a3ca743798e6a2138b588cda23051d34e5a6f028193798c7213430", 80170733);
  const a3 = await getAddrs("0xf2c2d7db1532c3ae9d38650753f776ace15b287e6e8bb2d6ccf2496f11e7bc71", 80141929);
  
  // Find the key addresses
  for (const a of a1) {
    if (a.startsWith("0xe3abc29b")) console.log("0xe3abc29b full:", a);
    if (a.startsWith("0xc096ff26")) console.log("0xc096ff26 full:", a);
    if (a.startsWith("0x29eeb257")) console.log("0x29eeb257 full:", a);
    if (a.startsWith("0x29ed0a2f")) console.log("0x29ed0a2f full:", a);
  }
  for (const a of a3) {
    if (a.startsWith("0x5f1e8ed8")) console.log("0x5f1e8ed8 full:", a);
    if (a.startsWith("0x152b9d0f")) console.log("0x152b9d0f full:", a);
  }
}
main().catch(console.error);
