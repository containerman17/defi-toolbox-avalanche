// WebSocket connection pool for JSON-RPC calls.
// Each worker owns one persistent WebSocket and handles one request at a time.
// Backpressure is natural: N workers = N max concurrent RPC calls.
//
// Usage as a viem transport:
//   import { wsPool, closePool } from "../../rpc/ws-pool.ts";
//   const client = createPublicClient({ chain, transport: wsPool(url) });

import { cpus } from "node:os";

// --- Types ---

interface JsonRpcRequest {
  jsonrpc: string;
  method: string;
  params: unknown[];
  id: number | string;
}

interface JsonRpcResponse {
  jsonrpc: string;
  id: number | string;
  result?: unknown;
  error?: { code: number; message: string; data?: unknown };
}

interface PendingRequest {
  payload: JsonRpcRequest;
  resolve: (resp: JsonRpcResponse) => void;
}

// --- WebSocket Worker ---

class WsWorker {
  ws!: WebSocket;
  pendingResolve: ((resp: JsonRpcResponse) => void) | null = null;
  ready = false;
  id: number;
  url: string;

  constructor(id: number, url: string) {
    this.id = id;
    this.url = url;
  }

  connect(): Promise<void> {
    return new Promise((resolve, reject) => {
      this.ws = new WebSocket(this.url);
      this.ws.onopen = () => {
        this.ready = true;
        resolve();
      };
      this.ws.onerror = (_e: Event) => {
        if (!this.ready) reject(new Error(`worker ${this.id}: connect failed`));
      };
      this.ws.onclose = () => {
        this.ready = false;
      };
      this.ws.onmessage = (event: MessageEvent) => {
        const data = JSON.parse(String(event.data)) as JsonRpcResponse;
        if (this.pendingResolve) {
          const r = this.pendingResolve;
          this.pendingResolve = null;
          r(data);
        }
      };
    });
  }

  call(req: JsonRpcRequest): Promise<JsonRpcResponse> {
    return new Promise((resolve) => {
      this.pendingResolve = resolve;
      this.ws.send(JSON.stringify(req));
    });
  }

  close() {
    this.ws.close();
  }
}

// --- Pool ---

class _WsPool {
  workers: WsWorker[] = [];
  queue: PendingRequest[] = [];
  freeWorkers: WsWorker[] = [];
  callCount = 0;
  url: string;
  numWorkers: number;

  constructor(url: string, numWorkers: number) {
    this.url = url;
    this.numWorkers = numWorkers;
  }

  async start() {
    const connects: Promise<void>[] = [];
    for (let i = 0; i < this.numWorkers; i++) {
      const w = new WsWorker(i, this.url);
      this.workers.push(w);
      connects.push(
        w.connect().then(() => {
          this.freeWorkers.push(w);
          this.drain();
        }),
      );
    }
    await Promise.all(connects);
    console.error(`WsPool: ${this.numWorkers} workers connected to ${this.url}`);
  }

  drain() {
    while (this.queue.length > 0 && this.freeWorkers.length > 0) {
      const req = this.queue.shift()!;
      const worker = this.freeWorkers.pop()!;
      this.dispatch(worker, req);
    }
  }

  async dispatch(worker: WsWorker, req: PendingRequest) {
    try {
      const resp = await worker.call(req.payload);
      this.callCount++;
      req.resolve(resp);
    } catch (err: any) {
      req.resolve({
        jsonrpc: "2.0",
        id: req.payload.id,
        error: { code: -32000, message: err.message ?? "worker error" },
      });
    } finally {
      this.freeWorkers.push(worker);
      this.drain();
    }
  }

  call(payload: JsonRpcRequest): Promise<JsonRpcResponse> {
    return new Promise((resolve) => {
      this.queue.push({ payload, resolve });
      this.drain();
    });
  }

  getCallCount() {
    return this.callCount;
  }

  close() {
    for (const w of this.workers) w.close();
  }
}

// --- viem-compatible custom transport (no viem import needed) ---

let _pool: _WsPool | null = null;

export function wsPool(url?: string, workers?: number) {
  const wsUrl = url ?? process.env.WS_URL ?? "ws://localhost:9650/ext/bc/C/ws";
  const numWorkers = workers ?? cpus().length;

  // Return a viem transport factory function
  return (opts: any) => {
    return {
      config: { type: "custom" as const },
      request: async ({ method, params }: { method: string; params: unknown[] }) => {
        if (!_pool) {
          _pool = new _WsPool(wsUrl, numWorkers);
          await _pool.start();
        }

        const resp = await _pool.call({
          jsonrpc: "2.0",
          method,
          params: params ?? [],
          id: _pool.callCount + 1,
        });

        if (resp.error) {
          const err = new Error(resp.error.message) as any;
          err.code = resp.error.code;
          err.data = resp.error.data;
          throw err;
        }

        return resp.result;
      },
      type: "custom" as const,
    };
  };
}

export function getPoolStats(): { calls: number } | null {
  if (!_pool) return null;
  return { calls: _pool.getCallCount() };
}

export function closePool() {
  if (_pool) {
    _pool.close();
    _pool = null;
  }
}
