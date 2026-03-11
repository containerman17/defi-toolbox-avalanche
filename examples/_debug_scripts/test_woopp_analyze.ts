import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const WOOPP = "0xaba7ed514217d51630053d73d358ac2502d3f9bb";
const block = BigInt(80108339) - 1n;

const code = await client.getBytecode({ address: WOOPP as `0x${string}`, blockNumber: block });
const hex = (code as string).slice(2);

// Find selector 0xac8bb7d9 in the jumptable
const target = "ac8bb7d9";
let pos = hex.indexOf(target);
while (pos !== -1) {
  console.log(`Found 0x${target} at byte offset ${pos / 2}: context = ${hex.slice(Math.max(0, pos-20), pos+40)}`);
  pos = hex.indexOf(target, pos + 1);
}
