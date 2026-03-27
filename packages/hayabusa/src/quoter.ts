import { spawn, type ChildProcess } from "node:child_process";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import type { NativeRouteResult } from "./types.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const BIN_DIR = join(__dirname, "..", "bin");

export interface BlockInfo {
  number: number;
  timestamp: number;
}

export interface QuoterOptions {
  /** WebSocket URL of the state server (e.g. "ws://localhost:7449/live") */
  stateServerUrl: string;
  /** Called when a new block arrives. Use this to re-quote on each block. */
  onBlock?: (block: BlockInfo) => void;
}

export interface FindRouteResult {
  steps: { pool: string; poolType: number; tokenIn: string; tokenOut: string }[];
  amountOut: bigint;
  stats: {
    formulaQuotes: number;
    evmQuotes: number;
    totalQuotes: number;
    formulaMs: number;
    evmMs: number;
  };
}

export interface Quoter {
  /** Find the best route from tokenIn to tokenOut */
  findRoute(tokenIn: string, tokenOut: string, amountIn: bigint): Promise<FindRouteResult | null>;

  /** Send a raw JSON-RPC request to the native harness */
  request(method: string, params: any): Promise<any>;

  /** Close the harness process */
  close(): void;
}

/**
 * Create a quoter backed by the native Go harness.
 *
 * The harness is a child process that communicates via stdin/stdout JSON-RPC.
 * It connects to a state server for live chain state and runs EVM + formula
 * quoting locally.
 *
 * @example
 * ```ts
 * const quoter = await createQuoter({ stateServerUrl: "ws://localhost:7449/live" });
 * const result = await quoter.findRoute(WAVAX, USDC, 10n ** 18n);
 * if (result) {
 *   console.log(`${result.amountOut} out via ${result.steps.length} hops`);
 * }
 * quoter.close();
 * ```
 */
export async function createQuoter(opts: QuoterOptions): Promise<Quoter> {
  const args = ["--state-server", opts.stateServerUrl];

  const child: ChildProcess = spawn(join(BIN_DIR, "harness-native"), args, {
    stdio: ["pipe", "pipe", "pipe"],
  });

  let nextId = 1;
  const pending = new Map<number, { resolve: (v: any) => void; reject: (e: Error) => void }>();
  let buffer = "";

  child.stdout!.on("data", (chunk: Buffer) => {
    buffer += chunk.toString();
    let nl: number;
    while ((nl = buffer.indexOf("\n")) !== -1) {
      const line = buffer.slice(0, nl).trim();
      buffer = buffer.slice(nl + 1);
      if (!line) continue;
      try {
        const msg = JSON.parse(line);
        // Block notification from Go harness (not a JSON-RPC response)
        if (msg.type === "block" && opts.onBlock) {
          opts.onBlock({ number: msg.blockNumber, timestamp: msg.timestamp });
          continue;
        }
        const p = pending.get(msg.id);
        if (p) {
          pending.delete(msg.id);
          if (msg.error) p.reject(new Error(typeof msg.error === "string" ? msg.error : JSON.stringify(msg.error)));
          else p.resolve(msg.result);
        }
      } catch {}
    }
  });

  // Wait for the harness to connect to the state server
  await new Promise<void>((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error("harness startup timeout (30s)")), 30_000);
    child.on("error", (err) => { clearTimeout(timeout); reject(err); });
    child.on("exit", (code) => {
      if (code !== null && code !== 0) {
        clearTimeout(timeout);
        reject(new Error(`harness exited with code ${code}`));
      }
    });
    // The harness prints "[native] connected to state server ..." to stderr when ready
    let stderrBuf = "";
    const onData = (chunk: Buffer) => {
      stderrBuf += chunk.toString();
      process.stderr.write(chunk); // pass through to caller's stderr
      if (stderrBuf.includes("connected to state server")) {
        child.stderr!.off("data", onData);
        clearTimeout(timeout);
        resolve();
      }
    };
    child.stderr!.on("data", onData);
  });

  function request(method: string, params: any): Promise<any> {
    return new Promise((resolve, reject) => {
      const id = nextId++;
      pending.set(id, { resolve, reject });
      child.stdin!.write(JSON.stringify({ id, method, params }) + "\n");
    });
  }

  async function findRoute(
    tokenIn: string,
    tokenOut: string,
    amountIn: bigint,
  ): Promise<FindRouteResult | null> {
    const result = await request("find_route", {
      tokenIn: tokenIn.toLowerCase(),
      tokenOut: tokenOut.toLowerCase(),
      amountIn: "0x" + amountIn.toString(16),
      maxHops: 2,
    });

    if (!result || result.route === null || !result.steps) {
      return null;
    }

    const native = result as NativeRouteResult;
    return {
      steps: native.steps,
      amountOut: BigInt(native.amountOut),
      stats: native.stats,
    };
  }

  function close() {
    child.stdin!.end();
    child.kill();
  }

  return { findRoute, request, close };
}
