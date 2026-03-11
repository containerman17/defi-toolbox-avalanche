import { keccak256, toHex } from "viem";
import { type CachedRPC } from "./types.ts";

function selector(sig: string): string {
  return keccak256(toHex(sig)).slice(0, 10);
}

export class CachedRpcClient implements CachedRPC {
  private rpcUrl: string;
  private cache = new Map<string, string>();
  private inflight = new Map<string, Promise<string>>();

  constructor(rpcUrl: string) {
    this.rpcUrl = rpcUrl;
  }

  async ethCall(to: string, method: string): Promise<string> {
    const cacheKey = `${to}:${method}`;

    const cached = this.cache.get(cacheKey);
    if (cached !== undefined) {
      if (cached.startsWith("ERROR:")) {
        throw new Error(cached.slice(6));
      }
      return cached;
    }

    const pending = this.inflight.get(cacheKey);
    if (pending) return pending;

    const fetchPromise = this.doFetch(to, method, cacheKey);
    this.inflight.set(cacheKey, fetchPromise);

    try {
      return await fetchPromise;
    } finally {
      this.inflight.delete(cacheKey);
    }
  }

  private async doFetch(
    to: string,
    method: string,
    cacheKey: string,
  ): Promise<string> {
    const data = selector(method);

    const response = await fetch(this.rpcUrl, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: 1,
        method: "eth_call",
        params: [{ to, data }, "latest"],
      }),
    });

    const json = (await response.json()) as {
      result?: string;
      error?: { message: string };
    };

    if (json.error) {
      const errorValue = `ERROR:${json.error.message}`;
      this.cache.set(cacheKey, errorValue);
      throw new Error(json.error.message);
    }

    const result = json.result ?? "0x";
    this.cache.set(cacheKey, result);
    return result;
  }

  async getAddress(contract: string, method: string): Promise<string> {
    const result = await this.ethCall(contract, method);
    if (!result || result === "0x" || result.length < 42)
      throw new Error("Invalid address result");
    return "0x" + result.slice(-40).toLowerCase();
  }

}
