import * as fs from "fs";
import { type StoredPool, type PoolType } from "./types.ts";

/**
 * Parse pools.txt content into a head block number and a Map of pools.
 * Format: line 1 = head block number
 * Lines 2+: address:providerName:poolType:latestSwapBlock:token0:token1[:@extraData]
 */
export function parsePools(content: string): {
  headBlock: number;
  pools: Map<string, StoredPool>;
} {
  const lines = content.split("\n").filter((l) => l.trim().length > 0);
  if (lines.length === 0) {
    return { headBlock: 0, pools: new Map() };
  }

  const headBlock = parseInt(lines[0]);
  if (isNaN(headBlock)) {
    throw new Error(`Invalid head block number: ${lines[0]}`);
  }

  const pools = new Map<string, StoredPool>();

  for (let i = 1; i < lines.length; i++) {
    const line = lines[i];
    const parts = line.split(":");
    if (parts.length < 6) continue;

    const address = parts[0].toLowerCase();
    if (!address.startsWith("0x") || address.length !== 42) continue;

    const providerName = parts[1];
    const poolType = parseInt(parts[2]) as PoolType;
    const latestSwapBlock = parseInt(parts[3]);
    if (isNaN(poolType) || isNaN(latestSwapBlock)) continue;

    // Tokens come after field 3. The last part may start with @ (extraData).
    const remaining = parts.slice(4);
    const tokens: string[] = [];
    let extraData: string | undefined;

    for (const part of remaining) {
      if (part.startsWith("@")) {
        extraData = part.slice(1); // strip leading @
      } else {
        tokens.push(part.toLowerCase());
      }
    }

    if (tokens.length < 2) continue;

    pools.set(address, {
      address,
      providerName,
      poolType,
      tokens,
      latestSwapBlock,
      extraData,
    });
  }

  return { headBlock, pools };
}

/** Load pools from a file. */
export function loadPools(filePath: string): {
  headBlock: number;
  pools: Map<string, StoredPool>;
} {
  if (!fs.existsSync(filePath)) {
    throw new Error(`Pools file not found: ${filePath}`);
  }
  return parsePools(fs.readFileSync(filePath, "utf-8"));
}

/** Serialize pools to the pools.txt format string. Sorted by latestSwapBlock descending. */
export function serializePools(
  headBlock: number,
  pools: Iterable<StoredPool>,
): string {
  const lines: string[] = [String(headBlock)];

  const sorted = Array.from(pools).sort(
    (a, b) => b.latestSwapBlock - a.latestSwapBlock,
  );

  for (const pool of sorted) {
    const tokens = pool.tokens.map((t) => t.toLowerCase()).sort();
    let line = `${pool.address.toLowerCase()}:${pool.providerName}:${pool.poolType}:${pool.latestSwapBlock}:${tokens.join(":")}`;
    if (pool.extraData) {
      line += `:@${pool.extraData}`;
    }
    lines.push(line);
  }

  return lines.join("\n") + "\n";
}

/** Save pools to a file. */
export function savePools(
  filePath: string,
  headBlock: number,
  pools: Iterable<StoredPool>,
): void {
  fs.writeFileSync(filePath, serializePools(headBlock, pools));
}

/**
 * Merge newly discovered pools into an existing pool map.
 * Updates latestSwapBlock if the new entry is more recent.
 */
export function mergePools(
  existing: Map<string, StoredPool>,
  newPools: Iterable<StoredPool>,
): void {
  for (const pool of newPools) {
    const key = pool.address.toLowerCase();
    const prev = existing.get(key);
    if (!prev) {
      existing.set(key, { ...pool, address: key });
    } else {
      // Merge tokens from both entries
      const mergedTokens = [...prev.tokens];
      for (const t of pool.tokens) {
        if (!mergedTokens.includes(t)) {
          mergedTokens.push(t);
        }
      }
      existing.set(key, {
        ...prev,
        tokens: mergedTokens,
        latestSwapBlock: Math.max(prev.latestSwapBlock, pool.latestSwapBlock),
        extraData: pool.extraData ?? prev.extraData,
      });
    }
  }
}
