import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const RPC_URL = process.env.RPC_URL || "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(RPC_URL) });

async function main() {
  const TRANSFER_TOPIC = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef";
  
  const trace = await (client as any).request({
    method: "debug_traceTransaction",
    params: ["0x447a7f6aca1d7593abec732829422fb0ab3bce338f6b628945cb0f02f6f7cc2c", 
             { tracer: "callTracer", tracerConfig: { withLog: true } }]
  });
  
  // Find the actual token address for the LP token between the two Platypus pools
  // We know from trace that 0xed2a7edd called 0xcfc... via transfer()
  // And 0xa196a03 also interacted with 0xcfc...
  
  function findCalls(call: any): any[] {
    const calls: any[] = [call];
    for (const sub of (call.calls || [])) {
      calls.push(...findCalls(sub));
    }
    return calls;
  }
  
  const allCalls = findCalls(trace);
  
  // Find calls to/from 0xed2a7edd or 0xa196a03
  const targets = [
    "0xed2a7edd7413021d440b09d654f3b87712abab66",
    "0xa196a03653f6cc5ca0282a8bd7ec60e93f620afc"
  ];
  
  for (const call of allCalls) {
    const toL = call.to?.toLowerCase();
    const fromL = call.from?.toLowerCase();
    if (targets.includes(toL) || targets.includes(fromL)) {
      // Print full details
      console.log(`${call.type} ${call.from} -> ${call.to}`);
      console.log(`  input: ${call.input?.slice(0, 100)}`);
      // Show logs
      if (call.logs) {
        for (const log of call.logs) {
          if (log.topics[0] === TRANSFER_TOPIC) {
            const from = "0x" + log.topics[1].slice(26);
            const to = "0x" + log.topics[2].slice(26);
            const amount = BigInt(log.data || "0x0");
            console.log(`  Transfer: ${log.address} | ${from} -> ${to} | ${amount}`);
          } else {
            console.log(`  Log: ${log.address} | ${log.topics[0]}`);
          }
        }
      }
      console.log();
    }
  }
  
  // Also look at what 0x91695586 args mean - decode them
  // POOL1 input: 0x91695586 + abi-encoded
  // Let's decode as (uint8, uint8, uint256, uint256, uint256)
  const pool1Input = "0x91695586000000000000000000000000000000000000000000000000000000000000000200000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000002044a8200000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000069b121c8";
  const pool2Input = "0x9169558600000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000002000000000000000000000000000000000000000000000001d608b002741f644900000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000069b121c8";
  
  // Decode: each arg is 32 bytes
  const decodeArgs = (input: string) => {
    const data = input.slice(10); // Remove selector
    const args = [];
    for (let i = 0; i < data.length; i += 64) {
      args.push(BigInt("0x" + data.slice(i, i + 64)));
    }
    return args;
  };
  
  console.log("POOL1 args:", decodeArgs(pool1Input).map(x => x.toString()));
  console.log("POOL2 args:", decodeArgs(pool2Input).map(x => x.toString()));
  // POOL1: fromIndex=2, toIndex=0, amount=33849602, minAmount=0, deadline
  // POOL2: fromIndex=0, toIndex=2, amount=big, minAmount=0, deadline
  
  // Wait, these are uint256 args but used as indices?
  // fromIndex=2 means USDC.e is at index 2 in the pool
  // toIndex=0 means LP/asset at index 0
  // Then pool2: fromIndex=0, toIndex=2 means LP→USDt
}

main().catch(console.error);
