import { createPublicClient, http } from "viem";
import { avalanche } from "viem/chains";

const rpcUrl = process.env.RPC_URL ?? "http://localhost:9650/ext/bc/C/rpc";
const client = createPublicClient({ chain: avalanche, transport: http(rpcUrl) });

const LFJ_SWAP = "0xd9a8cfa901e597f6bbb7ea94478cf9ad6f38d0dc3fd24d493e99cb40692e39f1";
const LFJ = "0x45a62b090df48243f12a21897e7ed91863e2c86b";

// CLI: node find_txs.ts [startBlock] [endBlock]
// Defaults scan forward from router deployment block until we have 1000 txs.
const ROUTER_DEPLOY_BLOCK = 80091636n;
const startBlock = process.argv[2] ? BigInt(process.argv[2]) : ROUTER_DEPLOY_BLOCK;
const targetCount = Number(process.argv[3] ?? 200);
const CHUNK_SIZE = 50000n; // blocks per query to avoid RPC limits

async function findSwaps(fromBlock: bigint, toBlock: bigint): Promise<{hash: string, block: number}[]> {
  const logs = await client.request({
    method: "eth_getLogs" as any,
    params: [{
      address: LFJ,
      topics: [LFJ_SWAP],
      fromBlock: `0x${fromBlock.toString(16)}`,
      toBlock: `0x${toBlock.toString(16)}`,
    }] as any,
  });

  const results: {hash: string, block: number}[] = [];
  const seen = new Set<string>();
  for (const log of (logs as any[])) {
    const hash = log.transactionHash.toLowerCase();
    if (seen.has(hash)) continue;
    seen.add(hash);
    results.push({ hash, block: Number(BigInt(log.blockNumber)) });
  }
  return results;
}

// Get latest block for upper bound
const latestHex = await client.request({ method: "eth_blockNumber" as any, params: [] as any });
const latestBlock = BigInt(latestHex as string);

const all: {hash: string, block: number}[] = [];
const seen = new Set<string>();
let cursor = startBlock;

while (all.length < targetCount && cursor < latestBlock) {
  const end = cursor + CHUNK_SIZE > latestBlock ? latestBlock : cursor + CHUNK_SIZE;
  process.stderr.write(`Scanning blocks ${cursor}..${end} (found ${all.length} so far)...\n`);

  const batch = await findSwaps(cursor, end);
  for (const tx of batch) {
    if (!seen.has(tx.hash)) {
      seen.add(tx.hash);
      all.push(tx);
    }
  }
  cursor = end + 1n;

  if (all.length >= targetCount) break;
}

all.sort((a, b) => a.block - b.block);
const output = all.slice(0, targetCount);

process.stderr.write(`Found ${output.length} LFJ swap txs across blocks ${output[0]?.block}..${output[output.length-1]?.block}\n`);

// Print as txs.txt format
console.log(`# LFJ aggregator swap txs for router e2e testing (${output.length} txs)`);
console.log("# Format: txHash  # block");
for (const tx of output) {
  console.log(`${tx.hash}  # block ${tx.block}`);
}
